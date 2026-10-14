// Package apps holds workload controller specs.
package apps

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("apps", "Deployment", func() {
	f := framework.NewDefaultFramework("deploy")

	It("should run the desired replicas as real containers spread across workers", func(ctx SpecContext) {
		name := f.UniqueName("spread")
		labels := map[string]string{"app": name}
		f.Apply(ctx, framework.NewDeployment(name, 2, labels, framework.DemoContainer()))

		pods := f.WaitForRunningPods(ctx, labels, 2)
		Expect(pods[0].Status.NodeName).NotTo(Equal(pods[1].Status.NodeName), "Pods should land on different workers")
		for _, p := range pods {
			Expect(p.Metadata.OwnerUID).NotTo(BeEmpty())
			Expect(p.Status.PodIP).NotTo(BeEmpty())
		}
	})

	It("should scale up and down with API Pods matching Docker containers", func(ctx SpecContext) {
		name := f.UniqueName("scale")
		labels := map[string]string{"app": name}
		f.Apply(ctx, framework.NewDeployment(name, 2, labels, framework.DemoContainer()))
		f.WaitForRunningPods(ctx, labels, 2)

		By("scaling to 3 replicas")
		framework.KK(ctx, "scale", "deployment", name, "--replicas", "3")
		f.WaitForRunningPods(ctx, labels, 3)

		By("scaling to 1 replica")
		framework.KK(ctx, "scale", "deployment", name, "--replicas", "1")
		f.WaitForRunningPods(ctx, labels, 1)

		By("scaling to 0 replicas")
		framework.KK(ctx, "scale", "deployment", name, "--replicas", "0")
		f.WaitForRunningPods(ctx, labels, 0)
	})

	It("should replace a deleted Pod with a new Pod", func(ctx SpecContext) {
		name := f.UniqueName("replace")
		labels := map[string]string{"app": name}
		f.Apply(ctx, framework.NewDeployment(name, 1, labels, framework.DemoContainer()))
		old := f.WaitForRunningPods(ctx, labels, 1)[0]

		framework.KK(ctx, "delete", "pod", old.Metadata.Name)
		Eventually(ctx, func(g Gomega) {
			pods, err := f.Pods(ctx, labels)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods).To(HaveLen(1))
			g.Expect(pods[0].Metadata.UID).NotTo(Equal(old.Metadata.UID))
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(Succeed())
		f.WaitForRunningPods(ctx, labels, 1)
		ids, err := framework.PodContainers(ctx, old.Metadata.UID, false)
		framework.ExpectNoError(err)
		Expect(ids).To(BeEmpty(), "the deleted Pod's container should be removed")
	})

	It("should garbage-collect its Pods and containers when deleted", func(ctx SpecContext) {
		name := f.UniqueName("gc")
		labels := map[string]string{"app": name}
		obj := framework.NewDeployment(name, 2, labels, framework.DemoContainer())
		f.Apply(ctx, obj)
		f.WaitForRunningPods(ctx, labels, 2)

		framework.KK(ctx, "delete", "deployment", name)
		f.DeleteAndWait(ctx, []framework.Object{obj})
		pods, err := f.Pods(ctx, labels)
		framework.ExpectNoError(err)
		Expect(pods).To(BeEmpty())
	})

	It("should reject template changes because only replicas are mutable", func(ctx SpecContext) {
		name := f.UniqueName("immutable")
		labels := map[string]string{"app": name}
		f.Apply(ctx, framework.NewDeployment(name, 1, labels, framework.DemoContainer()))

		changed := framework.DemoContainer()
		changed.Args = []string{"demo", "--changed"}
		out := f.ApplyExpectError(ctx, framework.NewDeployment(name, 1, labels, changed))
		Expect(out).To(ContainSubstring("immutable"))
	})
})
