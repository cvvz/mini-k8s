// Package lifecycle holds disruptive specs that restart or stop the cluster.
package lifecycle

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("cluster-lifecycle", "Control plane and cluster restarts", framework.WithDisruptive(), Serial, func() {
	f := framework.NewDefaultFramework("lifecycle")

	It("should keep desired state and resume reconciling after a control-plane restart", func(ctx SpecContext) {
		labels := map[string]string{"app": "cp"}
		f.Apply(ctx, framework.NewDeployment("cp", 1, labels, framework.DemoContainer()))
		f.WaitForRunningPods(ctx, labels, 1)
		before, err := client.Deployments(f.Client, f.Namespace).Get(ctx, "cp")
		framework.ExpectNoError(err)

		By("restarting apiserver, controller-manager and scheduler")
		_, err = framework.Docker(ctx, "restart",
			framework.ComponentContainer("apiserver"), framework.ComponentContainer("controller"), framework.ComponentContainer("scheduler"))
		framework.ExpectNoError(err)
		framework.WaitForClusterReady(ctx, 2)

		after, err := client.Deployments(f.Client, f.Namespace).Get(ctx, "cp")
		framework.ExpectNoError(err)
		Expect(after.UID).To(Equal(before.UID))

		By("scaling after the restart to prove the controllers are reconciling again")
		f.KK(ctx, "scale", "deployment", "cp", "--replicas", "2")
		f.WaitForRunningPods(ctx, labels, 2)
	})

	It("should recover a node's Pods after its node container restarts", func(ctx SpecContext) {
		labels := map[string]string{"app": "reboot"}
		obj := framework.NewPod("reboot", labels, framework.DemoContainer())
		obj.Spec()["nodeName"] = "worker-2"
		f.Apply(ctx, obj)
		before := f.WaitForRunningPods(ctx, labels, 1)[0]

		By("restarting the worker-2 node container, which kills its containerd and Pods")
		_, err := framework.Docker(ctx, "restart", framework.ComponentContainer("worker-2"))
		framework.ExpectNoError(err)
		framework.WaitForClusterReady(ctx, 2)

		after := f.WaitForPod(ctx, "reboot", "runs again in a new sandbox", func(p *corev1.Pod) bool {
			return corev1.IsPodReady(p) && len(p.Status.ContainerStatuses) == 1 &&
				p.Status.ContainerStatuses[0].ContainerID != before.Status.ContainerStatuses[0].ContainerID
		})
		Expect(after.UID).To(Equal(before.UID))
		Eventually(ctx, func() (string, error) {
			return framework.RunKK(ctx, "logs", "reboot", "-n", f.Namespace)
		}).Should(ContainSubstring("demo web server started"))
	})

	It("should remove workloads on cluster down and restore them on cluster up", func(ctx SpecContext) {
		labels := map[string]string{"app": "downup"}
		f.Apply(ctx, framework.NewDeployment("downup", 2, labels, framework.DemoContainer()))
		f.WaitForRunningPods(ctx, labels, 2)
		before, err := client.Deployments(f.Client, f.Namespace).Get(ctx, "downup")
		framework.ExpectNoError(err)

		By("stopping the cluster")
		framework.KK(ctx, "cluster", "down")
		out, err := framework.Docker(ctx, "ps", "-aq", "--filter", "label=com.docker.compose.project=mini-k8s")
		framework.ExpectNoError(err)
		Expect(out).To(BeEmpty(), "cluster down must remove every node and component container")

		By("starting the cluster again")
		framework.KK(ctx, "cluster", "up")
		framework.WaitForClusterReady(ctx, 2)
		after, err := client.Deployments(f.Client, f.Namespace).Get(ctx, "downup")
		framework.ExpectNoError(err)
		Expect(after.UID).To(Equal(before.UID), "desired state must survive down/up")
		f.WaitForRunningPods(ctx, labels, 2)
	})
})
