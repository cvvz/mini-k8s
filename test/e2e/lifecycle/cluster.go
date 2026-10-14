// Package lifecycle holds disruptive specs that restart or stop the cluster.
package lifecycle

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("cluster-lifecycle", "Control plane and cluster restarts", framework.WithDisruptive(), Serial, func() {
	f := framework.NewDefaultFramework("lifecycle")

	It("should keep desired state and resume reconciling after a control-plane restart", func(ctx SpecContext) {
		name := f.UniqueName("cp")
		labels := map[string]string{"app": name}
		f.Apply(ctx, framework.NewDeployment(name, 1, labels, framework.DemoContainer()))
		f.WaitForRunningPods(ctx, labels, 1)
		before, err := f.Client.Get(ctx, "Deployment", name)
		framework.ExpectNoError(err)

		By("restarting apiserver, controller and scheduler")
		_, err = framework.Docker(ctx, "restart",
			framework.ComponentContainer("apiserver"), framework.ComponentContainer("controller"), framework.ComponentContainer("scheduler"))
		framework.ExpectNoError(err)
		framework.WaitForClusterReady(ctx, 2)

		after, err := f.Client.Get(ctx, "Deployment", name)
		framework.ExpectNoError(err)
		Expect(after.Metadata.UID).To(Equal(before.Metadata.UID))

		By("scaling after the restart to prove the controllers are reconciling again")
		framework.KK(ctx, "scale", "deployment", name, "--replicas", "2")
		f.WaitForRunningPods(ctx, labels, 2)
	})

	It("should remove workloads on cluster down and restore them on cluster up", func(ctx SpecContext) {
		name := f.UniqueName("downup")
		labels := map[string]string{"app": name}
		f.Apply(ctx, framework.NewDeployment(name, 2, labels, framework.DemoContainer()))
		f.WaitForRunningPods(ctx, labels, 2)
		before, err := f.Client.Get(ctx, "Deployment", name)
		framework.ExpectNoError(err)

		By("stopping the cluster")
		framework.KK(ctx, "cluster", "down")
		out, err := framework.Docker(ctx, "ps", "-aq", "--filter", "label=mini.cluster=mini-k8s", "--filter", "label=mini.pod")
		framework.ExpectNoError(err)
		Expect(out).To(BeEmpty(), "cluster down must remove every workload container")

		By("starting the cluster again")
		framework.KK(ctx, "cluster", "up")
		framework.WaitForClusterReady(ctx, 2)
		after, err := f.Client.Get(ctx, "Deployment", name)
		framework.ExpectNoError(err)
		Expect(after.Metadata.UID).To(Equal(before.Metadata.UID), "desired state must survive down/up")
		f.WaitForRunningPods(ctx, labels, 2)
	})
})
