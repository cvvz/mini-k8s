// Package node holds kubelet and Node specs.
package node

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mini-k8s/internal/mini"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("node", "Nodes", func() {
	f := framework.NewDefaultFramework("nodes")

	It("should report every worker as Ready with a fresh heartbeat", func(ctx SpecContext) {
		nodes, err := f.Client.List(ctx, "Node")
		framework.ExpectNoError(err)
		names := map[string]bool{}
		for _, n := range nodes {
			Expect(framework.NodeReady(n, time.Now())).To(BeTrue(), "node %s: %+v", n.Metadata.Name, n.Status)
			names[n.Metadata.Name] = true
		}
		Expect(names).To(HaveKey("worker-1"))
		Expect(names).To(HaveKey("worker-2"))
		Expect(framework.KK(ctx, "cluster", "status")).To(ContainSubstring("API healthy"))
	})
})

var _ = framework.SIGDescribe("node", "Kubelet", func() {
	f := framework.NewDefaultFramework("kubelet")

	It("should restart a container that was killed and count the restart", func(ctx SpecContext) {
		name := f.UniqueName("restart")
		labels := map[string]string{"app": name}
		f.Apply(ctx, framework.NewPod(name, labels, framework.DemoContainer()))
		pod := f.WaitForRunningPods(ctx, labels, 1)[0]

		By("killing the Pod's container behind the kubelet's back")
		_, err := framework.Docker(ctx, "kill", pod.Status.ContainerID)
		framework.ExpectNoError(err)

		f.WaitForPod(ctx, name, "is Running again with an increased restart count", func(p mini.Resource) bool {
			return p.Status.Phase == "Running" && p.Status.Restarts > pod.Status.Restarts
		})
		f.WaitForRunningPods(ctx, labels, 1)
	})

	It("should serve container logs through the API", func(ctx SpecContext) {
		name := f.UniqueName("logs")
		labels := map[string]string{"app": name}
		f.Apply(ctx, framework.NewPod(name, labels, framework.DemoContainer()))
		f.WaitForRunningPods(ctx, labels, 1)

		Eventually(ctx, func() (string, error) {
			return framework.RunKK(ctx, "logs", name)
		}).WithTimeout(30 * time.Second).Should(ContainSubstring("demo web server started"))
	})

	It("should report image pull failures in the Pod status", func(ctx SpecContext) {
		name := f.UniqueName("badimage")
		c := framework.DemoContainer()
		c.Image = "mini-k8s-e2e/image-that-does-not-exist:0.0.0"
		f.Apply(ctx, framework.NewPod(name, map[string]string{"app": name}, c))

		f.WaitForPod(ctx, name, "reports ImageUnavailable", func(p mini.Resource) bool {
			return p.Status.Phase == "Failed" && strings.HasPrefix(p.Status.Reason, "ImageUnavailable")
		})
	})

	It("should back off a crash-looping container instead of restarting it hot", func(ctx SpecContext) {
		name := f.UniqueName("crashloop")
		f.Apply(ctx, framework.NewPod(name, map[string]string{"app": name}, framework.CrashingContainer()))

		pod := f.WaitForPod(ctx, name, "enters CrashLoopBackOff after restarting", func(p mini.Resource) bool {
			return p.Status.Restarts >= 2 && strings.HasPrefix(p.Status.Reason, "CrashLoopBackOff")
		})
		Expect(pod.Status.Phase).NotTo(Equal("Running"))
		// Each failed run of the same container appends its fatal log line.
		logs := framework.KK(ctx, "logs", name)
		Expect(strings.Count(logs, "\n")).To(BeNumerically(">=", 2), "logs:\n%s", logs)
	})
})
