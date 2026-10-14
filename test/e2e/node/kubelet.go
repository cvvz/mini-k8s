// Package node holds kubelet and Node specs.
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

var _ = framework.SIGDescribe("node", "Nodes", func() {
	f := framework.NewDefaultFramework("nodes")

	It("should report every worker as Ready with addresses and runtime info", func(ctx SpecContext) {
		nodes, err := client.Nodes(f.Client).List(ctx, client.ListOptions{})
		framework.ExpectNoError(err)
		names := map[string]bool{}
		for i := range nodes.Items {
			n := &nodes.Items[i]
			Expect(corev1.IsNodeReady(n, time.Now())).To(BeTrue(), "node %s: %+v", n.Name, n.Status)
			Expect(n.InternalIP()).NotTo(BeEmpty())
			Expect(n.Status.NodeInfo.ContainerRuntimeVersion).To(HavePrefix("docker://"))
			Expect(n.Labels).To(HaveKeyWithValue(corev1.LabelHostname, n.Name))
			names[n.Name] = true
		}
		Expect(names).To(HaveKey("worker-1"))
		Expect(names).To(HaveKey("worker-2"))
		Expect(framework.KK(ctx, "cluster", "status")).To(ContainSubstring("API healthy"))
		Expect(framework.KK(ctx, "get", "nodes")).To(MatchRegexp(`worker-1\s+Ready`))
	})
})

var _ = framework.SIGDescribe("node", "Kubelet", func() {
	f := framework.NewDefaultFramework("kubelet")

	It("should restart a container that was killed and count the restart", func(ctx SpecContext) {
		labels := map[string]string{"app": "restart"}
		f.Apply(ctx, framework.NewPod("restart", labels, framework.DemoContainer()))
		pod := f.WaitForRunningPods(ctx, labels, 1)[0]

		By("killing the Pod's container behind the kubelet's back")
		_, err := framework.Docker(ctx, "kill", framework.ContainerID(pod))
		framework.ExpectNoError(err)

		restarted := f.WaitForPod(ctx, "restart", "is Running again with an increased restart count", func(p *corev1.Pod) bool {
			return corev1.IsPodReady(p) && p.Status.ContainerStatuses[0].RestartCount > pod.Status.ContainerStatuses[0].RestartCount
		})
		Expect(restarted.Status.ContainerStatuses[0].LastTerminationState.Terminated).NotTo(BeNil())
		Expect(f.KK(ctx, "get", "pod", "restart")).To(MatchRegexp(`restart\s+1/1\s+Running\s+1`))
	})

	It("should serve container logs through the API, including tail and follow", func(ctx SpecContext) {
		labels := map[string]string{"app": "logs"}
		f.Apply(ctx, framework.NewPod("logs", labels, framework.DemoContainer()))
		f.WaitForRunningPods(ctx, labels, 1)

		Eventually(ctx, func() (string, error) {
			return framework.RunKK(ctx, "logs", "logs", "-n", f.Namespace)
		}).WithTimeout(30 * time.Second).Should(ContainSubstring("demo web server started"))
		Expect(strings.Count(f.KK(ctx, "logs", "logs", "--tail", "1"), "\n")).To(Equal(1))
	})

	It("should report image pull failures in the Pod status", func(ctx SpecContext) {
		c := framework.DemoContainer()
		c.Image = "mini-k8s-e2e/image-that-does-not-exist:0.0.0"
		f.Apply(ctx, framework.NewPod("badimage", map[string]string{"app": "badimage"}, c))

		pod := f.WaitForPod(ctx, "badimage", "reports an image pull error", func(p *corev1.Pod) bool {
			if len(p.Status.ContainerStatuses) == 0 || p.Status.ContainerStatuses[0].State.Waiting == nil {
				return false
			}
			r := p.Status.ContainerStatuses[0].State.Waiting.Reason
			return r == "ErrImagePull" || r == "ImagePullBackOff"
		})
		Expect(pod.Status.Phase).To(Equal(corev1.PodPending))
		Expect(f.KK(ctx, "get", "pods")).To(MatchRegexp(`badimage\s+0/1\s+(ErrImagePull|ImagePullBackOff)`))
		f.WaitForEvent(ctx, "badimage", "Failed")
	})

	It("should back off a crash-looping container instead of restarting it hot", func(ctx SpecContext) {
		f.Apply(ctx, framework.NewPod("crashloop", map[string]string{"app": "crashloop"}, framework.CrashingContainer()))

		pod := f.WaitForPod(ctx, "crashloop", "enters CrashLoopBackOff after restarting", func(p *corev1.Pod) bool {
			if len(p.Status.ContainerStatuses) == 0 {
				return false
			}
			cs := p.Status.ContainerStatuses[0]
			return cs.RestartCount >= 2 && cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff"
		})
		Expect(corev1.IsPodReady(pod)).To(BeFalse())
		Expect(pod.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode).NotTo(BeZero())
		f.WaitForEvent(ctx, "crashloop", "BackOff")
	})

	It("should terminate Pods gracefully with SIGTERM before removing them", func(ctx SpecContext) {
		c := framework.DemoContainer()
		c.Env = map[string]string{"DEMO_SHUTDOWN_DELAY": "4s"}
		f.Apply(ctx, framework.NewPod("graceful", map[string]string{"app": "graceful"}, c))
		pod := f.WaitForRunningPods(ctx, map[string]string{"app": "graceful"}, 1)[0]

		f.KK(ctx, "delete", "pod", "graceful", "--wait=false")
		By("observing the Pod as Terminating while the app drains")
		terminating := f.WaitForPod(ctx, "graceful", "is Terminating", func(p *corev1.Pod) bool { return p.DeletionTimestamp != nil })
		Expect(*terminating.DeletionGracePeriodSeconds).To(BeEquivalentTo(30))
		Expect(f.KK(ctx, "get", "pod", "graceful")).To(ContainSubstring("Terminating"))
		Expect(framework.PodContainers(ctx, pod.UID, true)).To(HaveLen(1), "the container keeps running during its grace period")
		f.WaitForPodGone(ctx, &pod, 30*time.Second)
		f.WaitForEvent(ctx, "graceful", "Killing")
	})

	It("should SIGKILL containers that ignore SIGTERM once the grace period expires", func(ctx SpecContext) {
		c := framework.DemoContainer()
		c.Env = map[string]string{"DEMO_IGNORE_SIGTERM": "1"}
		obj := framework.NewPod("stubborn", map[string]string{"app": "stubborn"}, c)
		obj.Spec()["terminationGracePeriodSeconds"] = 3
		f.Apply(ctx, obj)
		pod := f.WaitForRunningPods(ctx, map[string]string{"app": "stubborn"}, 1)[0]

		start := time.Now()
		f.KK(ctx, "delete", "pod", "stubborn")
		elapsed := time.Since(start)
		Expect(elapsed).To(BeNumerically(">=", 2*time.Second), "the kubelet must wait for the grace period")
		Expect(elapsed).To(BeNumerically("<", 20*time.Second), "the kubelet must not wait for the default 30s")
		f.WaitForPodGone(ctx, &pod, 10*time.Second)
	})

	It("should remove a force-deleted Pod immediately and clean up its container", func(ctx SpecContext) {
		f.Apply(ctx, framework.NewPod("forced", map[string]string{"app": "forced"}, framework.DemoContainer()))
		pod := f.WaitForRunningPods(ctx, map[string]string{"app": "forced"}, 1)[0]

		f.KK(ctx, "delete", "pod", "forced", "--force", "--wait=false")
		_, err := client.Pods(f.Client, f.Namespace).Get(ctx, "forced")
		Expect(client.IsNotFound(err)).To(BeTrue(), "force deletion removes the Pod from the API at once")
		f.WaitForPodGone(ctx, &pod, 30*time.Second)
	})
})
