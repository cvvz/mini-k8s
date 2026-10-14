package node

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

// httpProbe checks path on port 8080 every second.
func httpProbe(path string, failureThreshold int) map[string]any {
	return map[string]any{"httpGet": map[string]any{"path": path, "port": 8080}, "periodSeconds": 1, "failureThreshold": failureThreshold}
}

// eventMessages returns the messages of the object's Events with reason.
func eventMessages(ctx SpecContext, f *framework.Framework, name, reason string) []string {
	var out []string
	for _, e := range f.Events(ctx, name) {
		if e.Reason == reason {
			out = append(out, e.Message)
		}
	}
	return out
}

var _ = framework.SIGDescribe("node", "Probes", func() {
	f := framework.NewDefaultFramework("probes")

	// fetch calls the demo app inside the Pod.
	fetch := func(ctx SpecContext, pod, path string) {
		_, err := framework.RunKK(ctx, "exec", "-n", f.Namespace, pod, "--", "/mini", "fetch", "http://127.0.0.1:8080"+path)
		framework.ExpectNoError(err)
	}

	It("should restart a container whose liveness probe fails", func(ctx SpecContext) {
		c := framework.DemoContainer()
		c.Fields = map[string]any{"livenessProbe": httpProbe("/healthz", 2)}
		f.Apply(ctx, framework.NewPod("live", map[string]string{"app": "live"}, c))
		before := f.WaitForRunningPods(ctx, map[string]string{"app": "live"}, 1)[0]

		fetch(ctx, "live", "/fail-live")
		after := f.WaitForPod(ctx, "live", "is restarted and healthy again", func(p *corev1.Pod) bool {
			return corev1.IsPodReady(p) && p.Status.ContainerStatuses[0].RestartCount == 1
		})
		Expect(after.Status.ContainerStatuses[0].ContainerID).NotTo(Equal(before.Status.ContainerStatuses[0].ContainerID))
		Expect(eventMessages(ctx, f, "live", "Unhealthy")).To(ContainElement("Liveness probe failed: HTTP probe failed with statuscode: 500"))
		Expect(eventMessages(ctx, f, "live", "Killing")).To(ContainElement("Container web failed liveness probe, will be restarted"))
		Consistently(ctx, func() (int32, error) {
			p, err := client.Pods(f.Client, f.Namespace).Get(ctx, "live")
			if err != nil {
				return 0, err
			}
			return p.Status.ContainerStatuses[0].RestartCount, nil
		}).WithTimeout(4*time.Second).Should(BeEquivalentTo(1), "the new instance is healthy")
	})

	It("should hold off liveness checks until the startup probe passes", func(ctx SpecContext) {
		c := framework.DemoContainer()
		c.Env = map[string]string{"DEMO_START_DELAY": "6s"}
		tcp := func(failures int) map[string]any {
			return map[string]any{"tcpSocket": map[string]any{"port": 8080}, "periodSeconds": 1, "failureThreshold": failures}
		}
		c.Fields = map[string]any{"startupProbe": tcp(20), "livenessProbe": tcp(1), "readinessProbe": tcp(1)}
		f.Apply(ctx, framework.NewPod("slow", map[string]string{"app": "slow"}, c))

		starting := f.WaitForPod(ctx, "slow", "runs but has not started", func(p *corev1.Pod) bool {
			cs := p.Status.ContainerStatuses
			return len(cs) == 1 && cs[0].State.Running != nil && cs[0].Started != nil && !*cs[0].Started
		})
		Expect(starting.Status.ContainerStatuses[0].Ready).To(BeFalse())
		Expect(f.KK(ctx, "get", "pod", "slow")).To(MatchRegexp(`slow\s+0/1\s+Running\s+0`))

		started := f.WaitForPod(ctx, "slow", "started and ready", func(p *corev1.Pod) bool { return corev1.IsPodReady(p) })
		Expect(*started.Status.ContainerStatuses[0].Started).To(BeTrue())
		Expect(started.Status.ContainerStatuses[0].RestartCount).To(BeZero(), "liveness must not kill a container that is still starting")
	})

	It("should report containers whose readiness probe fails as not ready", func(ctx SpecContext) {
		web := framework.DemoContainer()
		web.Fields = map[string]any{"readinessProbe": map[string]any{
			"exec": map[string]any{"command": []string{"/mini", "cat", "/proc/1/status"}}, "periodSeconds": 1}}
		sidecar := framework.MiniContainer("sidecar", "pause")
		sidecar.Fields = map[string]any{"readinessProbe": map[string]any{"tcpSocket": map[string]any{"port": 9999}, "periodSeconds": 1}}
		f.Apply(ctx, framework.NewPod("half", map[string]string{"app": "half"}, web).WithContainers(web, sidecar))

		pod := f.WaitForPod(ctx, "half", "has a ready and an unready container", func(p *corev1.Pod) bool {
			cs := p.Status.ContainerStatuses
			return len(cs) == 2 && cs[0].Ready && cs[1].State.Running != nil && !cs[1].Ready
		})
		ready := pod.Status.Condition(corev1.ContainersReady)
		Expect(ready.Status).To(Equal(corev1.ConditionFalse))
		Expect(ready.Message).To(Equal("containers with unready status: [sidecar]"))
		Expect(f.KK(ctx, "get", "pod", "half")).To(MatchRegexp(`half\s+1/2\s+Running`))
		Eventually(ctx, func() []string { return eventMessages(ctx, f, "half", "Unhealthy") }).Should(ContainElement(
			And(HavePrefix("Readiness probe failed: dial tcp "), HaveSuffix(":9999: connect: connection refused"))))
	})
})
