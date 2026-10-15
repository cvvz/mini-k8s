package node

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("node", "Pod garbage collector", func() {
	f := framework.NewDefaultFramework("podgc")

	// Serial: the extra node would show up in other specs' scheduling
	// messages.
	It("should fail and delete the Pods of a node that no longer exists", Serial, func(ctx SpecContext) {
		name := "gone-" + f.Namespace
		nodes := client.Nodes(f.Client)
		_, err := nodes.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{Unschedulable: true}})
		framework.ExpectNoError(err)
		DeferCleanup(func(ctx SpecContext) {
			if err := nodes.Delete(ctx, name, nil); err != nil && !client.IsNotFound(err) {
				framework.ExpectNoError(err)
			}
		})
		pods := client.Pods(f.Client, f.Namespace)
		const hold = "e2e.mini-k8s.io/hold"
		for _, p := range []*corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "orphan"}},
			// The finalizer keeps the deleted Pod around to show its status.
			{ObjectMeta: metav1.ObjectMeta{Name: "held", Finalizers: []string{hold}}},
		} {
			p.Spec = corev1.PodSpec{NodeName: name, Containers: []corev1.Container{{Name: "c", Image: framework.DemoImage}}}
			_, err := pods.Create(ctx, p)
			framework.ExpectNoError(err)
		}
		DeferCleanup(func(ctx SpecContext) {
			p, err := pods.Get(ctx, "held")
			if client.IsNotFound(err) {
				return
			}
			framework.ExpectNoError(err)
			p.Finalizers = nil
			_, err = pods.Update(ctx, p)
			framework.ExpectNoError(err)
		})

		By("deleting the node")
		framework.ExpectNoError(nodes.Delete(ctx, name, nil))
		deleted := time.Now()
		Eventually(ctx, func() bool {
			_, err := pods.Get(ctx, "orphan")
			return client.IsNotFound(err)
		}).WithTimeout(2*time.Minute).WithPolling(time.Second).Should(BeTrue(), "the Pod of the vanished node is deleted")
		Expect(time.Since(deleted)).To(BeNumerically(">=", 40*time.Second), "after the 40s quarantine")

		held, err := pods.Get(ctx, "held")
		framework.ExpectNoError(err)
		Expect(held.DeletionTimestamp).NotTo(BeNil())
		Expect(held.Status.Phase).To(Equal(corev1.PodFailed))
		cond := held.Status.Condition(corev1.DisruptionTarget)
		Expect(cond).NotTo(BeNil())
		Expect(*cond).To(And(HaveField("Status", corev1.ConditionTrue), HaveField("Reason", "DeletionByPodGC"),
			HaveField("Message", "PodGC: node no longer exists")))
	})
})
