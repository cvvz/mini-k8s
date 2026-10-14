// Package apps holds workload controller specs.
package apps

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("apps", "Deployment", func() {
	f := framework.NewDefaultFramework("deploy")

	It("should run the desired replicas as real containers spread across workers", func(ctx SpecContext) {
		labels := map[string]string{"app": "spread"}
		f.Apply(ctx, framework.NewDeployment("spread", 2, labels, framework.DemoContainer()))

		pods := f.WaitForRunningPods(ctx, labels, 2)
		Expect(pods[0].Spec.NodeName).NotTo(Equal(pods[1].Spec.NodeName), "Pods should land on different workers")
		d, err := client.Deployments(f.Client, f.Namespace).Get(ctx, "spread")
		framework.ExpectNoError(err)
		for _, p := range pods {
			ref := metav1.ControllerOf(&p.ObjectMeta)
			Expect(ref).NotTo(BeNil())
			Expect(ref.UID).To(Equal(d.UID))
			Expect(p.Labels).To(HaveKey(appsv1.DefaultDeploymentUniqueLabelKey))
			Expect(p.Status.PodIP).NotTo(BeEmpty())
		}
		Eventually(ctx, func(g Gomega) {
			d, err := client.Deployments(f.Client, f.Namespace).Get(ctx, "spread")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(d.Status.ReadyReplicas).To(BeEquivalentTo(2))
			g.Expect(d.Status.AvailableReplicas).To(BeEquivalentTo(2))
			g.Expect(d.Status.ObservedGeneration).To(Equal(d.Generation))
		}).Should(Succeed())
		Expect(f.KK(ctx, "get", "deployment", "spread")).To(MatchRegexp(`spread\s+2/2\s+2\s+2`))
		f.WaitForEvent(ctx, "spread", "SuccessfulCreate")
	})

	It("should scale up and down with API Pods matching Docker containers", func(ctx SpecContext) {
		labels := map[string]string{"app": "scale"}
		f.Apply(ctx, framework.NewDeployment("scale", 2, labels, framework.DemoContainer()))
		f.WaitForRunningPods(ctx, labels, 2)

		By("scaling to 3 replicas")
		f.KK(ctx, "scale", "deployment", "scale", "--replicas", "3")
		f.WaitForRunningPods(ctx, labels, 3)

		By("scaling to 1 replica")
		f.KK(ctx, "scale", "deployment", "scale", "--replicas", "1")
		f.WaitForRunningPods(ctx, labels, 1)

		By("scaling to 0 replicas")
		f.KK(ctx, "scale", "deployment/scale", "--replicas", "0")
		f.WaitForRunningPods(ctx, labels, 0)
	})

	It("should replace a deleted Pod with a new Pod", func(ctx SpecContext) {
		labels := map[string]string{"app": "replace"}
		f.Apply(ctx, framework.NewDeployment("replace", 1, labels, framework.DemoContainer()))
		old := f.WaitForRunningPods(ctx, labels, 1)[0]

		f.KK(ctx, "delete", "pod", old.Name)
		f.WaitForPodGone(ctx, &old, framework.TestContext.PodStartTimeout)
		pods := f.WaitForRunningPods(ctx, labels, 1)
		Expect(pods[0].UID).NotTo(Equal(old.UID))
	})

	It("should recreate its Pods when the Pod template changes", func(ctx SpecContext) {
		labels := map[string]string{"app": "recreate"}
		obj := framework.NewDeployment("recreate", 2, labels, framework.DemoContainer())
		f.Apply(ctx, obj)
		before := f.WaitForRunningPods(ctx, labels, 2)

		By("changing the container environment")
		changed := framework.DemoContainer()
		changed.Env = map[string]string{"VERSION": "2"}
		Expect(f.Apply(ctx, framework.NewDeployment("recreate", 2, labels, changed))).To(ContainSubstring("configured"))
		Eventually(ctx, func(g Gomega) {
			pods, err := f.Pods(ctx, labels)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods).To(HaveLen(2))
			for _, p := range pods {
				g.Expect(p.Labels[appsv1.DefaultDeploymentUniqueLabelKey]).NotTo(Equal(before[0].Labels[appsv1.DefaultDeploymentUniqueLabelKey]))
				g.Expect(p.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{Name: "VERSION", Value: "2"}))
			}
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(Succeed())
		f.WaitForRunningPods(ctx, labels, 2)
		for _, p := range before {
			f.WaitForPodGone(ctx, &p, framework.TestContext.PodStartTimeout)
		}
	})

	It("should garbage-collect its Pods and containers when deleted", func(ctx SpecContext) {
		labels := map[string]string{"app": "gc"}
		f.Apply(ctx, framework.NewDeployment("gc", 2, labels, framework.DemoContainer()))
		pods := f.WaitForRunningPods(ctx, labels, 2)

		f.KK(ctx, "delete", "deployment", "gc")
		for _, p := range pods {
			f.WaitForPodGone(ctx, &p, framework.TestContext.PodStartTimeout)
		}
	})

	It("should leave Pods running without an owner when deleted with --cascade=orphan", func(ctx SpecContext) {
		labels := map[string]string{"app": "orphan"}
		f.Apply(ctx, framework.NewDeployment("orphan", 2, labels, framework.DemoContainer()))
		f.WaitForRunningPods(ctx, labels, 2)

		f.KK(ctx, "delete", "deployment", "orphan", "--cascade=orphan")
		_, err := client.Deployments(f.Client, f.Namespace).Get(ctx, "orphan")
		Expect(client.IsNotFound(err)).To(BeTrue())
		Eventually(ctx, func(g Gomega) {
			pods, err := f.Pods(ctx, labels)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods).To(HaveLen(2))
			for _, p := range pods {
				g.Expect(p.OwnerReferences).To(BeEmpty())
			}
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(Succeed())
		f.WaitForRunningPods(ctx, labels, 2)
	})

	It("should keep Pods while deleting in the foreground until dependents are gone", func(ctx SpecContext) {
		labels := map[string]string{"app": "fg"}
		f.Apply(ctx, framework.NewDeployment("fg", 1, labels, framework.DemoContainer()))
		pod := f.WaitForRunningPods(ctx, labels, 1)[0]

		f.KK(ctx, "delete", "deployment", "fg", "--cascade=foreground", "--wait=false")
		d, err := client.Deployments(f.Client, f.Namespace).Get(ctx, "fg")
		if err == nil {
			Expect(d.DeletionTimestamp).NotTo(BeNil())
			Expect(d.Finalizers).To(ContainElement(metav1.FinalizerDeleteDependents))
		}
		f.WaitForPodGone(ctx, &pod, framework.TestContext.PodStartTimeout)
		Eventually(ctx, func() bool {
			_, err := client.Deployments(f.Client, f.Namespace).Get(ctx, "fg")
			return client.IsNotFound(err)
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(BeTrue())
	})
})
