package node

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("node", "Resources", func() {
	f := framework.NewDefaultFramework("resources")

	limits := func(cpu, memory string) map[string]any {
		return map[string]any{"resources": map[string]any{"limits": map[string]any{"cpu": cpu, "memory": memory}}}
	}

	It("should enforce CPU and memory limits through the container's cgroup", func(ctx SpecContext) {
		c := framework.DemoContainer()
		c.Fields = limits("250m", "64Mi")
		f.Apply(ctx, framework.NewPod("limited", map[string]string{"app": "limited"}, c))
		pod := f.WaitForRunningPods(ctx, map[string]string{"app": "limited"}, 1)[0]
		Expect(pod.Status.QOSClass).To(Equal(corev1.PodQOSGuaranteed))
		out, err := framework.RunKK(ctx, "exec", "-n", f.Namespace, "limited", "--",
			"/mini", "cat", "/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory.swap.max", "/sys/fs/cgroup/cpu.max")
		framework.ExpectNoError(err)
		Expect(out).To(Equal("67108864\n0\n25000 100000\n"))
	})

	It("should OOM-kill a container that exceeds its memory limit", func(ctx SpecContext) {
		c := framework.MiniContainer("hog", "alloc", "256Mi")
		c.Fields = limits("500m", "32Mi")
		f.Apply(ctx, framework.NewPod("hog", map[string]string{"app": "hog"}, c).WithRestartPolicy(corev1.RestartPolicyNever))
		pod := f.WaitForPod(ctx, "hog", "is OOM-killed", func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodFailed })
		t := pod.Status.ContainerStatuses[0].State.Terminated
		Expect(t.Reason).To(Equal("OOMKilled"))
		Expect(t.ExitCode).To(BeEquivalentTo(137))
		Expect(f.KK(ctx, "get", "pod", "hog")).To(MatchRegexp(`hog\s+0/1\s+OOMKilled\s+0`))
	})

	It("should classify Pods into QoS classes", func(ctx SpecContext) {
		requests := framework.MiniContainer("app", "pause")
		requests.Fields = map[string]any{"resources": map[string]any{"requests": map[string]any{"memory": "16Mi"}}}
		guaranteed := framework.MiniContainer("app", "pause")
		guaranteed.Fields = limits("100m", "16Mi")
		f.Apply(ctx,
			framework.NewPod("besteffort", map[string]string{"app": "q1"}, framework.MiniContainer("app", "pause")),
			framework.NewPod("burstable", map[string]string{"app": "q2"}, requests),
			framework.NewPod("guaranteed", map[string]string{"app": "q3"}, guaranteed))
		for name, want := range map[string]string{"besteffort": corev1.PodQOSBestEffort, "burstable": corev1.PodQOSBurstable, "guaranteed": corev1.PodQOSGuaranteed} {
			p, err := client.Pods(f.Client, f.Namespace).Get(ctx, name)
			framework.ExpectNoError(err)
			Expect(p.Status.QOSClass).To(Equal(want), name)
		}
		g, err := client.Pods(f.Client, f.Namespace).Get(ctx, "guaranteed")
		framework.ExpectNoError(err)
		r := g.Spec.Containers[0].Resources.Requests
		Expect(r).To(HaveKey("cpu"), "requests default to limits")
		Expect(f.ApplyExpectError(ctx, framework.NewPod("bad", map[string]string{"app": "bad"}, func() framework.Container {
			c := framework.MiniContainer("app", "pause")
			c.Fields = map[string]any{"resources": map[string]any{"limits": map[string]any{"cpu": "1"}, "requests": map[string]any{"cpu": "2"}}}
			return c
		}()))).To(ContainSubstring(`must be less than or equal to cpu limit of 1`))
	})

	It("should report node capacity", func(ctx SpecContext) {
		nodes, err := client.Nodes(f.Client).List(ctx, client.ListOptions{})
		framework.ExpectNoError(err)
		for _, n := range nodes.Items {
			cpu, memory, pods := n.Status.Capacity["cpu"], n.Status.Capacity["memory"], n.Status.Allocatable["pods"]
			Expect(cpu.Value()).To(BeNumerically(">=", 1), n.Name)
			Expect(memory.Value()).To(BeNumerically(">", 256<<20), n.Name)
			Expect(pods.Value()).To(BeEquivalentTo(110), n.Name)
		}
		Expect(framework.KK(ctx, "describe", "node", "worker-1")).To(ContainSubstring("capacity:"))
	})
})
