package apps

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("apps", "DaemonSet", func() {
	f := framework.NewDefaultFramework("daemonset")

	newDaemonSet := func(name string, env string) framework.Object {
		c := framework.MiniContainer("c", "sleep", "1h")
		c.Env = map[string]string{"VERSION": env}
		labels := map[string]string{"app": name}
		return framework.Object{"apiVersion": "apps/v1", "kind": "DaemonSet", "metadata": map[string]any{"name": name},
			"spec": map[string]any{"selector": map[string]any{"matchLabels": labels},
				"template": map[string]any{"metadata": map[string]any{"labels": labels},
					"spec": map[string]any{"terminationGracePeriodSeconds": 1, "containers": []any{framework.ContainerObject(c)}}}}}
	}
	// live maps the nodes of the DaemonSet's live Pods to their revision.
	live := func(ctx SpecContext, name string) map[string]string {
		list, err := client.Pods(f.Client, f.Namespace).List(ctx, client.ListOptions{LabelSelector: "app=" + name})
		framework.ExpectNoError(err)
		out := map[string]string{}
		for _, p := range list.Items {
			if p.DeletionTimestamp == nil && p.Spec.NodeName != "" && corev1.IsPodReady(&p) {
				out[p.Spec.NodeName] = p.Labels[appsv1.ControllerRevisionHashLabelKey]
			}
		}
		return out
	}
	nodesOf := func(ctx SpecContext, name string) []string {
		var nodes []string
		for n := range live(ctx, name) {
			nodes = append(nodes, n)
		}
		return nodes
	}

	It("should run a Pod on every matching node and follow node labels", func(ctx SpecContext) {
		key := "e2e.mini-k8s.io/" + f.Namespace
		framework.KK(ctx, "label", "nodes", "worker-1", key+"=yes")
		DeferCleanup(func(ctx SpecContext) {
			for _, n := range framework.Workers {
				_, _ = framework.RunKK(ctx, "label", "nodes", n, key+"-")
			}
		})
		ds := newDaemonSet("agent", "1")
		ds["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["nodeSelector"] = map[string]any{key: "yes"}
		f.Apply(ctx, ds)
		Eventually(ctx, func() []string { return nodesOf(ctx, "agent") }).WithTimeout(time.Minute).Should(ConsistOf("worker-1"))
		Eventually(ctx, func() string { return f.KK(ctx, "get", "daemonset", "agent") }).
			Should(MatchRegexp(`(?m)^agent\s+1\s+1\s+1\s+1\s+1\s+` + key + `=yes\s+`))

		By("labelling worker-2: it gets a Pod too")
		framework.KK(ctx, "label", "nodes", "worker-2", key+"=yes")
		Eventually(ctx, func() []string { return nodesOf(ctx, "agent") }).WithTimeout(time.Minute).Should(ConsistOf("worker-1", "worker-2"))

		By("unlabelling worker-1: its Pod goes")
		framework.KK(ctx, "label", "nodes", "worker-1", key+"-")
		Eventually(ctx, func() []string { return nodesOf(ctx, "agent") }).WithTimeout(time.Minute).Should(ConsistOf("worker-2"))

		By("deleting the Pod on worker-2: the DaemonSet replaces it")
		list, err := client.Pods(f.Client, f.Namespace).List(ctx, client.ListOptions{LabelSelector: "app=agent"})
		framework.ExpectNoError(err)
		var old string
		for _, p := range list.Items {
			if p.Spec.NodeName == "worker-2" && p.DeletionTimestamp == nil {
				old = p.Name
				f.KK(ctx, "delete", "pod", p.Name)
			}
		}
		Eventually(ctx, func(g Gomega) {
			list, err := client.Pods(f.Client, f.Namespace).List(ctx, client.ListOptions{LabelSelector: "app=agent"})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(list.Items).To(ContainElement(And(HaveField("Spec.NodeName", "worker-2"), Not(HaveField("Name", old)))))
		}).WithTimeout(time.Minute).Should(Succeed())
		Expect(f.Events(ctx, "agent")).To(ContainElement(HaveField("Reason", "SuccessfulCreate")))
	})

	It("should roll out a template change node by node and roll it back", func(ctx SpecContext) {
		f.Apply(ctx, newDaemonSet("roller", "1"))
		Expect(f.KK(ctx, "rollout", "status", "daemonset/roller", "--timeout", "90s")).To(HaveSuffix("daemon set \"roller\" successfully rolled out\n"))
		first := live(ctx, "roller")
		Expect(first).To(HaveLen(2), "one Pod per worker; the control plane's taint keeps it away")
		Expect(first["worker-1"]).To(Equal(first["worker-2"]))
		Expect(f.KK(ctx, "get", "controllerrevisions")).To(MatchRegexp(`(?m)^roller-` + first["worker-1"] + `\s+daemonset.apps/roller\s+1\s+`))

		By("changing the template")
		f.Apply(ctx, newDaemonSet("roller", "2"))
		out := f.KK(ctx, "rollout", "status", "daemonset/roller", "--timeout", "120s")
		Expect(out).To(HaveSuffix("daemon set \"roller\" successfully rolled out\n"))
		second := live(ctx, "roller")
		Expect(second).To(HaveLen(2))
		Expect(second["worker-1"]).NotTo(Equal(first["worker-1"]))
		Expect(second["worker-2"]).To(Equal(second["worker-1"]))
		Expect(f.KK(ctx, "rollout", "history", "daemonset/roller")).To(MatchRegexp(`REVISION\s+CHANGE-CAUSE\n1\s+<none>\n2\s+<none>`))

		By("rolling back")
		Expect(f.KK(ctx, "rollout", "undo", "daemonset/roller")).To(Equal("daemonset.apps/roller rolled back\n"))
		f.KK(ctx, "rollout", "status", "daemonset/roller", "--timeout", "120s")
		Eventually(ctx, func() map[string]string { return live(ctx, "roller") }).WithTimeout(time.Minute).Should(Equal(first))
		Expect(f.KK(ctx, "rollout", "history", "daemonset/roller")).To(MatchRegexp(`REVISION\s+CHANGE-CAUSE\n2\s+<none>\n3\s+<none>`))
		Expect(f.KK(ctx, "rollout", "history", "daemonset/roller", "--revision", "3")).To(ContainSubstring("value: \"1\""))
	})

	It("should start new Pods before stopping old ones with maxSurge", func(ctx SpecContext) {
		ds := newDaemonSet("surger", "1")
		ds["spec"].(map[string]any)["updateStrategy"] = map[string]any{"type": "RollingUpdate",
			"rollingUpdate": map[string]any{"maxSurge": 1, "maxUnavailable": 0}}
		f.Apply(ctx, ds)
		f.KK(ctx, "rollout", "status", "daemonset/surger", "--timeout", "90s")
		before := live(ctx, "surger")["worker-1"]
		ds = newDaemonSet("surger", "2")
		ds["spec"].(map[string]any)["updateStrategy"] = map[string]any{"type": "RollingUpdate",
			"rollingUpdate": map[string]any{"maxSurge": 1, "maxUnavailable": 0}}
		f.Apply(ctx, ds)
		// Every node keeps a ready Pod throughout; at some point a node has
		// an old and a new one.
		sawSurge := false
		Eventually(ctx, func(g Gomega) {
			list, err := client.Pods(f.Client, f.Namespace).List(ctx, client.ListOptions{LabelSelector: "app=surger"})
			g.Expect(err).NotTo(HaveOccurred())
			ready, perNode := map[string]bool{}, map[string]int{}
			for _, p := range list.Items {
				if p.DeletionTimestamp == nil && p.Spec.NodeName != "" {
					perNode[p.Spec.NodeName]++
				}
				if corev1.IsPodReady(&p) && p.DeletionTimestamp == nil {
					ready[p.Spec.NodeName] = true
				}
			}
			Expect(ready).To(HaveLen(2), "no node is left without a ready Pod")
			for _, n := range perNode {
				sawSurge = sawSurge || n == 2
			}
			ds, err := client.DaemonSets(f.Client, f.Namespace).Get(ctx, "surger")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ds.Status.ObservedGeneration).To(Equal(ds.Generation))
			now := live(ctx, "surger")
			g.Expect(now).To(HaveLen(2))
			g.Expect(now["worker-1"]).NotTo(Equal(before), "the Pods run the new revision")
			g.Expect(now["worker-2"]).To(Equal(now["worker-1"]))
			g.Expect(ds.Status.UpdatedNumberScheduled).To(BeEquivalentTo(2))
			g.Expect(ds.Status.NumberAvailable).To(BeEquivalentTo(2))
			g.Expect(perNode).To(Equal(map[string]int{"worker-1": 1, "worker-2": 1}))
		}).WithTimeout(2 * time.Minute).WithPolling(200 * time.Millisecond).Should(Succeed())
		Expect(sawSurge).To(BeTrue(), "a node ran the old and the new Pod at once")
	})
})
