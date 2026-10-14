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

var _ = framework.SIGDescribe("node", "Pods with several containers", func() {
	f := framework.NewDefaultFramework("pods")

	// kkExec runs `kk exec` in the spec namespace; flags go before "--".
	kkExec := func(ctx SpecContext, pod string, args ...string) (string, error) {
		return framework.RunKK(ctx, append([]string{"exec", "-n", f.Namespace, pod}, args...)...)
	}
	podLabels := func(name string) map[string]string { return map[string]string{"app": name} }
	newPod := func(name string, cs ...framework.Container) framework.Object {
		return framework.NewPod(name, podLabels(name), cs[0]).WithContainers(cs...)
	}

	It("should run containers that share the Pod's network and keep separate logs", func(ctx SpecContext) {
		f.Apply(ctx, newPod("duo", framework.DemoContainer(), framework.MiniContainer("sidecar", "pause")))
		f.WaitForRunningPods(ctx, podLabels("duo"), 1)
		Expect(f.KK(ctx, "get", "pod", "duo")).To(MatchRegexp(`duo\s+2/2\s+Running\s+0`))

		By("calling the web container from the sidecar over localhost")
		Eventually(ctx, func() (string, error) {
			return kkExec(ctx, "duo", "-c", "sidecar", "--", "/mini", "fetch", "http://127.0.0.1:8080/shared")
		}).WithTimeout(20 * time.Second).Should(Equal("200\nhello from duo\n"))

		Expect(f.KK(ctx, "logs", "duo", "-c", "web")).To(ContainSubstring("GET /shared"))
		Expect(f.KK(ctx, "logs", "duo", "-c", "sidecar")).To(BeEmpty())
		Expect(f.KK(ctx, "logs", "duo")).To(And(
			ContainSubstring(`Defaulted container "web" out of: web, sidecar`), ContainSubstring("GET /shared")))
	})

	It("should run init containers one after another before the containers", func(ctx SpecContext) {
		f.Apply(ctx, newPod("ordered", framework.DemoContainer()).WithInitContainers(
			framework.MiniContainer("wait", "sleep", "4s"),
			framework.MiniContainer("check", "fetch", "http://apiserver:8080/healthz")))

		By("observing the Pod while the first init container runs")
		Eventually(ctx, func() (string, error) {
			return framework.RunKK(ctx, "get", "pod", "ordered", "-n", f.Namespace)
		}).WithTimeout(20 * time.Second).WithPolling(200 * time.Millisecond).Should(MatchRegexp(`ordered\s+0/1\s+Init:0/2`))

		pod := f.WaitForRunningPods(ctx, podLabels("ordered"), 1)[0]
		wait, check := pod.Status.InitContainerStatuses[0], pod.Status.InitContainerStatuses[1]
		for _, cs := range []corev1.ContainerStatus{wait, check} {
			Expect(cs.State.Terminated).NotTo(BeNil(), "init container %s: %+v", cs.Name, cs.State)
			Expect(cs.State.Terminated.Reason).To(Equal("Completed"))
			Expect(cs.Ready).To(BeTrue())
		}
		Expect(check.State.Terminated.StartedAt.Time).NotTo(BeTemporally("<", wait.State.Terminated.FinishedAt.Time.Truncate(time.Second)))
		Expect(pod.Status.ContainerStatuses[0].State.Running.StartedAt.Time).NotTo(
			BeTemporally("<", check.State.Terminated.FinishedAt.Time.Truncate(time.Second)))
		Expect(f.KK(ctx, "logs", "ordered", "-c", "check")).To(HavePrefix("401"), "init containers resolve and reach the API")
		Expect(pod.Status.Condition(corev1.PodInitialized).Status).To(Equal(corev1.ConditionTrue))
	})

	It("should fail the Pod when an init container fails under restartPolicy Never", func(ctx SpecContext) {
		f.Apply(ctx, newPod("initfail", framework.DemoContainer()).
			WithInitContainers(framework.CrashingContainer()).WithRestartPolicy(corev1.RestartPolicyNever))
		pod := f.WaitForPod(ctx, "initfail", "fails", func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodFailed })
		Expect(pod.Status.InitContainerStatuses[0].State.Terminated.ExitCode).To(BeEquivalentTo(1))
		Expect(pod.Status.ContainerStatuses[0].State.Waiting.Reason).To(Equal("PodInitializing"))
		Expect(pod.Status.Condition(corev1.PodReady).Reason).To(Equal("PodFailed"))
		Expect(f.KK(ctx, "get", "pod", "initfail")).To(MatchRegexp(`initfail\s+0/1\s+Init:Error`))
		Expect(framework.PodContainers(ctx, pod.UID, true)).To(BeEmpty(), "the app container never starts")
	})

	It("should retry a failing init container with backoff under restartPolicy Always", func(ctx SpecContext) {
		f.Apply(ctx, newPod("initretry", framework.DemoContainer()).WithInitContainers(framework.CrashingContainer()))
		f.WaitForPod(ctx, "initretry", "backs off its init container", func(p *corev1.Pod) bool {
			if len(p.Status.InitContainerStatuses) == 0 {
				return false
			}
			cs := p.Status.InitContainerStatuses[0]
			return cs.RestartCount >= 2 && cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" &&
				p.Status.Phase == corev1.PodPending
		})
		Expect(f.KK(ctx, "get", "pod", "initretry")).To(MatchRegexp(`initretry\s+0/1\s+Init:CrashLoopBackOff`))
	})

	It("should finish Pods for good under restartPolicy Never", func(ctx SpecContext) {
		f.Apply(ctx,
			newPod("succeeds", framework.MiniContainer("work", "sleep", "1s")).WithRestartPolicy(corev1.RestartPolicyNever),
			newPod("fails", framework.CrashingContainer()).WithRestartPolicy(corev1.RestartPolicyNever))
		done := f.WaitForPod(ctx, "succeeds", "succeeds", func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodSucceeded })
		failed := f.WaitForPod(ctx, "fails", "fails", func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodFailed })
		Expect(f.KK(ctx, "get", "pod", "succeeds")).To(MatchRegexp(`succeeds\s+0/1\s+Completed\s+0`))
		Expect(f.KK(ctx, "get", "pod", "fails")).To(MatchRegexp(`fails\s+0/1\s+Error\s+0`))

		By("checking that nothing restarts and the sandboxes are stopped")
		Consistently(ctx, func() int32 {
			p, err := client.Pods(f.Client, f.Namespace).Get(ctx, "fails")
			framework.ExpectNoError(err)
			return p.Status.ContainerStatuses[0].RestartCount
		}).WithTimeout(4 * time.Second).Should(BeZero())
		for _, p := range []*corev1.Pod{done, failed} {
			Eventually(ctx, func() (string, error) {
				return framework.Crictl(ctx, p.Spec.NodeName, "pods", "-q", "--state", "ready", "--label", "io.kubernetes.pod.uid="+p.UID)
			}).WithTimeout(10*time.Second).Should(BeEmpty(), "sandbox of finished Pod %s", p.Name)
		}

		_, err := kkExec(ctx, "succeeds", "--", "/mini", "fetch", "http://127.0.0.1:8080/")
		Expect(err).To(MatchError(ContainSubstring("cannot exec into a container in a completed pod; current phase is Succeeded")))
	})

	It("should restart only failed containers under restartPolicy OnFailure", func(ctx SpecContext) {
		f.Apply(ctx, newPod("onfailure", framework.MiniContainer("done", "sleep", "1s"), framework.CrashingContainer()).
			WithRestartPolicy(corev1.RestartPolicyOnFailure))
		pod := f.WaitForPod(ctx, "onfailure", "restarts the failed container only", func(p *corev1.Pod) bool {
			if len(p.Status.ContainerStatuses) != 2 {
				return false
			}
			done, crash := p.Status.ContainerStatuses[0], p.Status.ContainerStatuses[1]
			return done.State.Terminated != nil && done.State.Terminated.ExitCode == 0 && crash.RestartCount >= 1
		})
		Expect(pod.Status.ContainerStatuses[0].RestartCount).To(BeZero())
		Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
	})

	It("should run commands with kk exec and pass on their exit code", func(ctx SpecContext) {
		f.Apply(ctx, newPod("shell", framework.DemoContainer()))
		f.WaitForRunningPods(ctx, podLabels("shell"), 1)
		Eventually(ctx, func() (string, error) {
			return kkExec(ctx, "shell", "--", "/mini", "fetch", "http://127.0.0.1:8080/exec")
		}).WithTimeout(20 * time.Second).Should(Equal("200\nhello from shell\n"))

		out, err := kkExec(ctx, "shell", "--", "/mini", "fetch", "http://127.0.0.1:1/")
		Expect(err).To(HaveOccurred())
		Expect(out).To(And(ContainSubstring("connection refused"), ContainSubstring("command terminated with exit code 1")))

		_, err = kkExec(ctx, "shell", "-c", "nope", "--", "/mini")
		Expect(err).To(MatchError(ContainSubstring("container nope is not valid for pod shell")))
	})

	It("should reject Deployments whose Pods would not restart", func(ctx SpecContext) {
		d := framework.NewDeployment("never", 1, podLabels("never"), framework.DemoContainer()).WithRestartPolicy(corev1.RestartPolicyNever)
		Expect(f.ApplyExpectError(ctx, d)).To(ContainSubstring(`spec.template.spec.restartPolicy: Unsupported value: "Never": supported values: "Always"`))
	})

	It("should stop all containers in parallel within one grace period", func(ctx SpecContext) {
		stubborn := func(name, port string) framework.Container {
			c := framework.DemoContainer()
			c.Name, c.Env = name, map[string]string{"DEMO_IGNORE_SIGTERM": "1", "DEMO_PORT": port}
			return c
		}
		obj := newPod("stubborn2", stubborn("a", "8080"), stubborn("b", "8081"))
		obj.Spec()["terminationGracePeriodSeconds"] = 3
		f.Apply(ctx, obj)
		pod := f.WaitForRunningPods(ctx, podLabels("stubborn2"), 1)[0]

		start := time.Now()
		f.KK(ctx, "delete", "pod", "stubborn2")
		elapsed := time.Since(start)
		Expect(elapsed).To(BeNumerically(">=", 2*time.Second), "the kubelet waits for the grace period")
		Expect(elapsed).To(BeNumerically("<", 6*time.Second), "both containers get the same 3s grace period, not one after the other")
		f.WaitForPodGone(ctx, &pod, 10*time.Second)
		Expect(strings.Count(f.KK(ctx, "get", "events", "--field-selector", "involvedObject.name=stubborn2"), "Killing")).To(Equal(2))
	})
})
