package node

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("node", "Node lifecycle", func() {
	f := framework.NewDefaultFramework("lifecycle")

	nodeOf := func(ctx SpecContext, name string) *corev1.Node {
		GinkgoHelper()
		n, err := client.Nodes(f.Client).Get(ctx, name)
		framework.ExpectNoError(err)
		return n
	}
	podsOn := func(ctx SpecContext, labels map[string]string, node string) []corev1.Pod {
		pods, err := f.Pods(ctx, labels)
		framework.ExpectNoError(err)
		var out []corev1.Pod
		for _, p := range pods {
			if p.Spec.NodeName == node && p.DeletionTimestamp == nil {
				out = append(out, p)
			}
		}
		return out
	}
	// onWorker2 runs the workload on worker-2 alone: worker-1 is cordoned
	// while its Pods are scheduled.
	onWorker2 := func(ctx SpecContext, labels map[string]string, replicas int, objs ...framework.Object) []corev1.Pod {
		GinkgoHelper()
		framework.KK(ctx, "cordon", "worker-1")
		DeferCleanup(func(ctx SpecContext) { framework.KK(ctx, "uncordon", "worker-1") })
		f.Apply(ctx, objs...)
		pods := f.WaitForRunningPods(ctx, labels, replicas)
		framework.KK(ctx, "uncordon", "worker-1")
		for _, p := range pods {
			Expect(p.Spec.NodeName).To(Equal("worker-2"))
		}
		return pods
	}
	taintKeys := func(n *corev1.Node) []string {
		var out []string
		for _, t := range n.Spec.Taints {
			out = append(out, t.Key+":"+t.Effect)
		}
		return out
	}

	It("should renew a Lease for every node in kube-node-lease", func(ctx SpecContext) {
		renewed := map[string]time.Time{}
		for _, node := range framework.Nodes {
			l, err := client.Leases(f.Client, corev1.NamespaceNodeLease).Get(ctx, node)
			framework.ExpectNoError(err, node)
			Expect(*l.Spec.HolderIdentity).To(Equal(node))
			Expect(*l.Spec.LeaseDurationSeconds).To(BeNumerically(">", 0))
			Expect(l.OwnerReferences).To(ConsistOf(HaveField("Kind", "Node")))
			renewed[node] = l.Spec.RenewTime.Time
		}
		Eventually(ctx, func(g Gomega) {
			for _, node := range framework.Nodes {
				l, err := client.Leases(f.Client, corev1.NamespaceNodeLease).Get(ctx, node)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(l.Spec.RenewTime.After(renewed[node])).To(BeTrue(), "lease of %s renewed", node)
			}
		}).WithTimeout(10 * time.Second).Should(Succeed())
		Expect(framework.KK(ctx, "get", "leases", "-n", corev1.NamespaceNodeLease)).To(MatchRegexp(`worker-1\s+worker-1\s+`))
	})

	It("should give every Pod the default tolerations for lost nodes", func(ctx SpecContext) {
		labels := map[string]string{"app": "tolerant"}
		f.Apply(ctx, framework.NewPod("tolerant", labels, framework.MiniContainer("c", "sleep", "1h")))
		p := f.WaitForRunningPods(ctx, labels, 1)[0]
		var got []string
		for _, t := range p.Spec.Tolerations {
			got = append(got, t.Key+":"+t.Effect+":"+t.Operator)
			Expect(*t.TolerationSeconds).To(BeEquivalentTo(300))
		}
		Expect(got).To(ConsistOf(corev1.TaintNodeNotReady+":NoExecute:Exists", corev1.TaintNodeUnreachable+":NoExecute:Exists"))
	})

	It("should evict Pods that do not tolerate a NoExecute taint", framework.WithDisruptive(), Serial, func(ctx SpecContext) {
		labels := map[string]string{"app": "evictee"}
		onWorker2(ctx, labels, 2, framework.NewDeployment("evictee", 2, labels, framework.DemoContainer()))
		stay := framework.NewPod("stay", map[string]string{"app": "stay"}, framework.MiniContainer("c", "sleep", "1h"))
		stay.Spec()["nodeName"] = "worker-2"
		stay.Spec()["tolerations"] = []any{map[string]any{"key": "e2e-evict", "operator": "Exists", "effect": "NoExecute"}}
		brief := framework.NewPod("brief", map[string]string{"app": "brief"}, framework.MiniContainer("c", "sleep", "1h"))
		brief.Spec()["nodeName"] = "worker-2"
		brief.Spec()["tolerations"] = []any{map[string]any{"key": "e2e-evict", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 3}}
		f.Apply(ctx, stay, brief)
		f.WaitForRunningPods(ctx, map[string]string{"app": "stay"}, 1)
		f.WaitForRunningPods(ctx, map[string]string{"app": "brief"}, 1)

		Expect(framework.KK(ctx, "taint", "nodes", "worker-2", "e2e-evict=true:NoExecute")).To(Equal("node/worker-2 tainted\n"))
		DeferCleanup(func(ctx SpecContext) { framework.KK(ctx, "taint", "nodes", "worker-2", "e2e-evict-") })
		Eventually(ctx, func() []corev1.Pod { return podsOn(ctx, labels, "worker-1") }).
			WithTimeout(framework.TestContext.PodStartTimeout).Should(HaveLen(2), "the evicted replicas come back on worker-1")
		f.WaitForRunningPods(ctx, labels, 2)
		Eventually(ctx, func() []corev1.Pod { return podsOn(ctx, map[string]string{"app": "brief"}, "worker-2") }).
			WithTimeout(30*time.Second).Should(BeEmpty(), "evicted after tolerationSeconds")
		Expect(podsOn(ctx, map[string]string{"app": "stay"}, "worker-2")).To(HaveLen(1), "a Pod tolerating the taint stays")
		var reasons []string
		for _, e := range f.Events(ctx, "brief") {
			reasons = append(reasons, e.Reason+": "+e.Message)
		}
		Expect(reasons).To(ContainElement("TaintManagerEviction: Marking for deletion Pod " + f.Namespace + "/brief"))
	})

	It("should drain a node, keeping its workloads running elsewhere", framework.WithDisruptive(), Serial, func(ctx SpecContext) {
		labels := map[string]string{"app": "drained"}
		onWorker2(ctx, labels, 2, framework.NewDeployment("drained", 2, labels, framework.DemoContainer()))
		loner := framework.NewPod("loner", map[string]string{"app": "loner"}, framework.MiniContainer("c", "sleep", "1h"))
		loner.Spec()["nodeName"] = "worker-2"
		f.Apply(ctx, loner)
		f.WaitForRunningPods(ctx, map[string]string{"app": "loner"}, 1)
		DeferCleanup(func(ctx SpecContext) { framework.KK(ctx, "uncordon", "worker-2") })

		out, err := framework.RunKK(ctx, "drain", "worker-2")
		Expect(err).To(HaveOccurred())
		Expect(out).To(And(ContainSubstring("node/worker-2 cordoned"),
			ContainSubstring("cannot delete Pods declare no controller (use --force to override): "+f.Namespace+"/loner")))
		Expect(framework.KK(ctx, "get", "nodes", "worker-2")).To(MatchRegexp(`worker-2\s+Ready,SchedulingDisabled`))

		out = framework.KK(ctx, "drain", "worker-2", "--force", "--ignore-daemonsets", "--timeout", "90s")
		Expect(out).To(And(ContainSubstring("node/worker-2 already cordoned"), ContainSubstring("evicting pod "+f.Namespace+"/loner"),
			ContainSubstring("pod/loner evicted"), HaveSuffix("node/worker-2 drained\n")))
		Expect(podsOn(ctx, labels, "worker-2")).To(BeEmpty())
		Eventually(ctx, func() []corev1.Pod { return podsOn(ctx, labels, "worker-1") }).
			WithTimeout(framework.TestContext.PodStartTimeout).Should(HaveLen(2))
		f.WaitForRunningPods(ctx, labels, 2)
		Expect(out).To(MatchRegexp(`Warning: ignoring DaemonSet-managed Pods: kube-system/kube-proxy-\w+`))
		proxies, err := client.Pods(f.Client, "kube-system").List(ctx, client.ListOptions{LabelSelector: "k8s-app=kube-proxy",
			FieldSelector: "spec.nodeName=worker-2"})
		framework.ExpectNoError(err)
		Expect(proxies.Items).To(HaveLen(1), "DaemonSet Pods stay on a drained node")

		Expect(framework.KK(ctx, "uncordon", "worker-2")).To(Equal("node/worker-2 uncordoned\n"))
	})

	It("should mark a lost node NotReady, taint it and evict its Pods after their toleration", framework.WithDisruptive(), Serial, func(ctx SpecContext) {
		labels := map[string]string{"app": "survivor"}
		d := framework.NewDeployment("survivor", 2, labels, framework.DemoContainer())
		d.PodSpec()["tolerations"] = []any{map[string]any{"key": corev1.TaintNodeUnreachable, "operator": "Exists",
			"effect": "NoExecute", "tolerationSeconds": 5}}
		lost := onWorker2(ctx, labels, 2, d, framework.NewService("survivor", labels, 8080))

		By("stopping worker-2's node container")
		_, err := framework.Docker(ctx, "stop", framework.ComponentContainer("worker-2"))
		framework.ExpectNoError(err)
		DeferCleanup(func(ctx SpecContext) {
			_, _ = framework.Docker(ctx, "start", framework.ComponentContainer("worker-2"))
			framework.WaitForClusterReady(ctx, len(framework.Nodes))
		})
		Eventually(ctx, func(g Gomega) {
			n := nodeOf(ctx, "worker-2")
			ready := n.Status.Condition(corev1.NodeReady)
			g.Expect(ready.Status).To(Equal(corev1.ConditionUnknown))
			g.Expect(ready.Reason).To(Equal("NodeStatusUnknown"))
			g.Expect(ready.Message).To(Equal("Kubelet stopped posting node status."))
			g.Expect(taintKeys(n)).To(ContainElements(corev1.TaintNodeUnreachable+":NoSchedule", corev1.TaintNodeUnreachable+":NoExecute"))
		}).WithTimeout(60 * time.Second).WithPolling(time.Second).Should(Succeed())
		Expect(framework.KK(ctx, "get", "nodes", "worker-2")).To(MatchRegexp(`worker-2\s+NotReady`))
		Eventually(ctx, func() string { return framework.KK(ctx, "get", "events", "-n", "default") }).
			Should(MatchRegexp(`NodeNotReady\s+node/worker-2\s+Node worker-2 status is now: NodeNotReady`))

		By("taking its Pod out of the Service and evicting it after 5s")
		Eventually(ctx, func() string { return framework.KK(ctx, "get", "endpoints", "survivor", "-n", f.Namespace) }).
			WithTimeout(30 * time.Second).ShouldNot(ContainSubstring(lost[0].Status.PodIP + ":"))
		Eventually(ctx, func(g Gomega) {
			for _, l := range lost {
				p, err := client.Pods(f.Client, f.Namespace).Get(ctx, l.Name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(p.DeletionTimestamp).NotTo(BeNil(), "marked for deletion; it terminates once the node is back")
			}
		}).WithTimeout(60 * time.Second).Should(Succeed())
		Eventually(ctx, func() []corev1.Pod { return podsOn(ctx, labels, "worker-1") }).
			WithTimeout(framework.TestContext.PodStartTimeout).Should(HaveLen(2))

		By("starting worker-2 again")
		_, err = framework.Docker(ctx, "start", framework.ComponentContainer("worker-2"))
		framework.ExpectNoError(err)
		Eventually(ctx, func(g Gomega) {
			n := nodeOf(ctx, "worker-2")
			g.Expect(corev1.IsNodeReady(n)).To(BeTrue())
			for _, k := range taintKeys(n) {
				g.Expect(strings.HasPrefix(k, "node.kubernetes.io/")).To(BeFalse(), "taint %s", k)
			}
		}).WithTimeout(2 * time.Minute).WithPolling(time.Second).Should(Succeed())
		for i := range lost {
			f.WaitForPodGone(ctx, &lost[i], 2*time.Minute)
		}
	})
})
