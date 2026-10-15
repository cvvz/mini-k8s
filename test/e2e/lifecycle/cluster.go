// Package lifecycle holds disruptive specs that restart or stop the cluster.
package lifecycle

import (
	"net"
	"time"

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

		By("restarting the control-plane node, which stops etcd and every control-plane component")
		_, err = framework.Docker(ctx, "restart", framework.ComponentContainer("control-plane"))
		framework.ExpectNoError(err)
		framework.WaitForClusterReady(ctx, len(framework.Nodes))

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
		never := framework.NewPod("reboot-never", map[string]string{"app": "reboot-never"}, framework.DemoContainer()).
			WithRestartPolicy(corev1.RestartPolicyNever)
		never.Spec()["nodeName"] = "worker-2"
		f.Apply(ctx, obj, never)
		before := f.WaitForRunningPods(ctx, labels, 1)[0]
		f.WaitForRunningPods(ctx, map[string]string{"app": "reboot-never"}, 1)

		By("restarting the worker-2 node container, which kills its containerd and Pods")
		_, err := framework.Docker(ctx, "restart", framework.ComponentContainer("worker-2"))
		framework.ExpectNoError(err)
		framework.WaitForClusterReady(ctx, len(framework.Nodes))

		after := f.WaitForPod(ctx, "reboot", "runs again in a new sandbox", func(p *corev1.Pod) bool {
			return corev1.IsPodReady(p) && len(p.Status.ContainerStatuses) == 1 &&
				p.Status.ContainerStatuses[0].ContainerID != before.Status.ContainerStatuses[0].ContainerID
		})
		Expect(after.UID).To(Equal(before.UID))
		Eventually(ctx, func() (string, error) {
			return framework.RunKK(ctx, "logs", "reboot", "-n", f.Namespace)
		}).Should(ContainSubstring("demo web server started"))

		By("checking that a restartPolicy Never Pod is not run again but fails")
		lost := f.WaitForPod(ctx, "reboot-never", "fails", func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodFailed })
		Expect(lost.Status.ContainerStatuses[0].State.Terminated.Reason).To(Equal("ContainerStatusUnknown"))
		Expect(framework.PodContainers(ctx, lost.UID, true)).To(BeEmpty())
	})

	It("should add a worker with kk cluster up --workers and drain it away again", func(ctx SpecContext) {
		scaledBack := false
		DeferCleanup(func(ctx SpecContext) {
			if !scaledBack {
				framework.KK(ctx, "cluster", "up", "--workers", "2")
			}
		})
		By("scaling out to three workers")
		out := framework.KK(ctx, "cluster", "up", "--workers", "3")
		Expect(out).To(ContainSubstring("nodes control-plane, worker-1, worker-2, worker-3 Ready"))
		node, err := client.Nodes(f.Client).Get(ctx, "worker-3")
		framework.ExpectNoError(err)
		Expect(corev1.IsNodeReady(node)).To(BeTrue())
		Expect(node.Labels).To(HaveKeyWithValue(corev1.LabelHostname, "worker-3"))
		_, podCIDR, err := net.ParseCIDR(node.Spec.PodCIDR)
		framework.ExpectNoError(err)

		By("running a Pod that prefers the new worker and reaching it from worker-1")
		labels := map[string]string{"app": "newcomer"}
		d := framework.NewDeployment("newcomer", 1, labels, framework.DemoContainer())
		d.PodSpec()["affinity"] = map[string]any{"nodeAffinity": map[string]any{"preferredDuringSchedulingIgnoredDuringExecution": []any{
			map[string]any{"weight": 100, "preference": map[string]any{"matchExpressions": []any{
				map[string]any{"key": corev1.LabelHostname, "operator": "In", "values": []any{"worker-3"}}}}}}}}
		probe := framework.NewPod("probe", map[string]string{"app": "probe"}, framework.DemoContainer())
		probe.Spec()["nodeName"] = "worker-1"
		f.Apply(ctx, d, probe)
		pod := f.WaitForRunningPods(ctx, labels, 1)[0]
		Expect(pod.Spec.NodeName).To(Equal("worker-3"))
		Expect(podCIDR.Contains(net.ParseIP(pod.Status.PodIP))).To(BeTrue(), "Pod IP %s outside %s", pod.Status.PodIP, podCIDR)
		src := f.WaitForRunningPods(ctx, map[string]string{"app": "probe"}, 1)[0]
		Eventually(ctx, func() (string, error) {
			return framework.ExecInPod(ctx, src, "/mini", "fetch", "http://"+pod.Status.PodIP+":8080/newcomer")
		}).WithTimeout(30 * time.Second).Should(HavePrefix("200\n"))

		By("scaling back to two workers, which drains worker-3 first")
		out = framework.KK(ctx, "cluster", "up", "--workers", "2")
		scaledBack = true
		Expect(out).To(ContainSubstring("Draining worker-3"))
		Expect(out).To(ContainSubstring("evicting pod " + f.Namespace + "/" + pod.Name))
		Expect(out).To(ContainSubstring("Deleting node worker-3"))
		_, err = client.Nodes(f.Client).Get(ctx, "worker-3")
		Expect(client.IsNotFound(err)).To(BeTrue(), "the Node object must be deleted: %v", err)
		Expect(framework.Docker(ctx, "ps", "-aq", "--filter", "name=^mini-k8s-worker-3-1$")).To(BeEmpty())
		Expect(framework.Docker(ctx, "volume", "ls", "-q", "--filter", "name=^mini-k8s_worker-3-var$")).To(BeEmpty())
		moved := f.WaitForRunningPods(ctx, labels, 1)[0]
		Expect(moved.UID).NotTo(Equal(pod.UID))
		Expect(moved.Spec.NodeName).To(BeElementOf("worker-1", "worker-2"))
		framework.ExpectDefaultNodes(ctx)
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
		framework.WaitForClusterReady(ctx, len(framework.Nodes))
		after, err := client.Deployments(f.Client, f.Namespace).Get(ctx, "downup")
		framework.ExpectNoError(err)
		Expect(after.UID).To(Equal(before.UID), "desired state must survive down/up")
		f.WaitForRunningPods(ctx, labels, 2)
	})
})
