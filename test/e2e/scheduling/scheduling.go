// Package scheduling holds scheduler specs: node selection, resource fit,
// spreading and inter-Pod affinity.
package scheduling

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("scheduling", "Scheduler", func() {
	f := framework.NewDefaultFramework("sched")

	// label sets a node label for the spec and removes it afterwards.
	label := func(ctx SpecContext, node, key, value string) {
		GinkgoHelper()
		framework.KK(ctx, "label", "nodes", node, key+"="+value, "--overwrite")
		DeferCleanup(func(ctx SpecContext) { framework.KK(ctx, "label", "nodes", node, key+"-") })
	}
	pod := func(name string, spec map[string]any) framework.Object {
		p := framework.NewPod(name, map[string]string{"app": name}, framework.MiniContainer("c", "sleep", "1h"))
		for k, v := range spec {
			p.Spec()[k] = v
		}
		return p
	}
	running := func(ctx SpecContext, name string) corev1.Pod {
		GinkgoHelper()
		return f.WaitForRunningPods(ctx, map[string]string{"app": name}, 1)[0]
	}
	// pendingWith waits until the Pod is unschedulable with a message.
	pendingWith := func(ctx SpecContext, name string, matcher OmegaMatcher) {
		GinkgoHelper()
		Eventually(ctx, func(g Gomega) {
			p, err := client.Pods(f.Client, f.Namespace).Get(ctx, name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(p.Spec.NodeName).To(BeEmpty())
			c := p.Status.Condition(corev1.PodScheduled)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal("Unschedulable"))
			g.Expect(c.Message).To(matcher)
		}).WithTimeout(30 * time.Second).Should(Succeed())
	}
	nodesOf := func(ctx SpecContext, labels map[string]string) map[string]int {
		pods, err := f.Pods(ctx, labels)
		framework.ExpectNoError(err)
		out := map[string]int{}
		for _, p := range pods {
			if p.Spec.NodeName != "" && p.DeletionTimestamp == nil {
				out[p.Spec.NodeName]++
			}
		}
		return out
	}
	const cpTaint = "1 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }"

	It("should place Pods by nodeSelector and node affinity, and schedule a waiting Pod once a node matches", func(ctx SpecContext) {
		key := "e2e.mini-k8s.io/" + f.Namespace
		label(ctx, "worker-2", key, "chosen")
		f.Apply(ctx,
			pod("selected", map[string]any{"nodeSelector": map[string]any{key: "chosen"}}),
			pod("avoider", map[string]any{"affinity": map[string]any{"nodeAffinity": map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{"nodeSelectorTerms": []any{
					map[string]any{"matchExpressions": []any{map[string]any{"key": key, "operator": "NotIn", "values": []any{"chosen"}}}}}}}}}),
			pod("preferrer", map[string]any{"affinity": map[string]any{"nodeAffinity": map[string]any{
				"preferredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{"weight": 100, "preference": map[string]any{
					"matchExpressions": []any{map[string]any{"key": key, "operator": "Exists"}}}}}}}}),
			pod("waiting", map[string]any{"nodeSelector": map[string]any{key: "later"}}))
		Expect(running(ctx, "selected").Spec.NodeName).To(Equal("worker-2"))
		Expect(running(ctx, "avoider").Spec.NodeName).To(Equal("worker-1"))
		Expect(running(ctx, "preferrer").Spec.NodeName).To(Equal("worker-2"))
		pendingWith(ctx, "waiting", Equal("0/3 nodes are available: "+cpTaint+", 2 node(s) didn't match Pod's node affinity/selector."))
		Expect(f.Events(ctx, "waiting")).To(ContainElement(HaveField("Reason", "FailedScheduling")))

		By("labelling worker-1 to match the waiting Pod")
		label(ctx, "worker-1", key, "later")
		Expect(running(ctx, "waiting").Spec.NodeName).To(Equal("worker-1"))
	})

	It("should account for resource requests", Serial, func(ctx SpecContext) {
		var milli int64
		for _, name := range framework.Workers {
			n, err := client.Nodes(f.Client).Get(ctx, name)
			framework.ExpectNoError(err)
			q := n.Status.Allocatable[corev1.ResourceCPU]
			if milli == 0 || q.MilliValue() < milli {
				milli = q.MilliValue()
			}
		}
		share := fmt.Sprintf("%dm", milli*6/10) // more than half a worker
		c := framework.MiniContainer("c", "sleep", "1h")
		c.Fields = map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": share}}}
		labels := map[string]string{"app": "hog"}
		f.Apply(ctx, framework.NewDeployment("hog", 2, labels, c))
		f.WaitForRunningPods(ctx, labels, 2)
		Expect(nodesOf(ctx, labels)).To(Equal(map[string]int{"worker-1": 1, "worker-2": 1}), "one per worker: two do not fit on one")

		big := pod("too-big", map[string]any{"containers": []any{map[string]any{"name": "c", "image": framework.DemoImage,
			"command": []any{"/mini"}, "args": []any{"sleep", "1h"},
			"resources": map[string]any{"requests": map[string]any{"cpu": fmt.Sprintf("%dm", milli+1000)}}}}})
		f.Apply(ctx, big)
		pendingWith(ctx, "too-big", Equal("0/3 nodes are available: "+cpTaint+", 2 Insufficient cpu."))

		By("scaling past what fits")
		framework.KK(ctx, "scale", "-n", f.Namespace, "deployment", "hog", "--replicas", "3")
		Eventually(ctx, func() int {
			pods, _ := f.Pods(ctx, labels)
			return len(pods)
		}).Should(Equal(3))
		Consistently(ctx, func() map[string]int { return nodesOf(ctx, labels) }).WithTimeout(4*time.Second).
			Should(Equal(map[string]int{"worker-1": 1, "worker-2": 1}), "the third replica waits for room")
	})

	It("should spread a Deployment's replicas across the workers", func(ctx SpecContext) {
		labels := map[string]string{"app": "spread"}
		f.Apply(ctx, framework.NewDeployment("spread", 4, labels, framework.MiniContainer("c", "sleep", "1h")))
		f.WaitForRunningPods(ctx, labels, 4)
		Expect(nodesOf(ctx, labels)).To(Equal(map[string]int{"worker-1": 2, "worker-2": 2}))
	})

	It("should keep replicas apart with required pod anti-affinity", func(ctx SpecContext) {
		labels := map[string]string{"app": "apart"}
		d := framework.NewDeployment("apart", 3, labels, framework.MiniContainer("c", "sleep", "1h"))
		d.PodSpec()["affinity"] = map[string]any{"podAntiAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": []any{
			map[string]any{"labelSelector": map[string]any{"matchLabels": labels}, "topologyKey": corev1.LabelHostname}}}}
		f.Apply(ctx, d)
		Eventually(ctx, func() map[string]int { return nodesOf(ctx, labels) }).WithTimeout(framework.TestContext.PodStartTimeout).
			Should(Equal(map[string]int{"worker-1": 1, "worker-2": 1}))
		Eventually(ctx, func(g Gomega) {
			pods, err := f.Pods(ctx, labels)
			g.Expect(err).NotTo(HaveOccurred())
			var waiting []corev1.Pod
			for _, p := range pods {
				if p.Spec.NodeName == "" {
					waiting = append(waiting, p)
				}
			}
			g.Expect(waiting).To(HaveLen(1))
			c := waiting[0].Status.Condition(corev1.PodScheduled)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Message).To(Equal("0/3 nodes are available: " + cpTaint + ", 2 node(s) didn't satisfy existing pods anti-affinity rules."))
		}).WithTimeout(30 * time.Second).Should(Succeed())
	})

	It("should schedule a Pod as soon as a cordoned node is uncordoned", framework.WithDisruptive(), Serial, func(ctx SpecContext) {
		for _, w := range framework.Workers {
			framework.KK(ctx, "cordon", w)
			DeferCleanup(func(ctx SpecContext) { framework.KK(ctx, "uncordon", w) })
		}
		f.Apply(ctx, pod("patient", nil))
		pendingWith(ctx, "patient", Equal("0/3 nodes are available: "+cpTaint+", 2 node(s) were unschedulable."))
		framework.KK(ctx, "uncordon", "worker-2")
		start := time.Now()
		Expect(running(ctx, "patient").Spec.NodeName).To(Equal("worker-2"))
		Expect(time.Since(start)).To(BeNumerically("<", 30*time.Second))
	})
})
