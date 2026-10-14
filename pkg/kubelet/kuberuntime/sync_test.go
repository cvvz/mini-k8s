package kuberuntime

import (
	"context"
	"strings"
	"testing"
	"time"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// multiPod has init containers a and b and containers x and y.
func multiPod(policy string) *corev1.Pod {
	pod := testPod()
	c := func(name string) corev1.Container { return corev1.Container{Name: name, Image: "img:1"} }
	pod.Spec.InitContainers = []corev1.Container{c("a"), c("b")}
	pod.Spec.Containers = []corev1.Container{c("x"), c("y")}
	pod.Spec.RestartPolicy = policy
	return pod
}

func waitingReason(cs corev1.ContainerStatus) string {
	if cs.State.Waiting == nil {
		return ""
	}
	return cs.State.Waiting.Reason
}

func condition(st corev1.PodStatus, t string) corev1.PodCondition {
	if c := st.Condition(t); c != nil {
		return *c
	}
	return corev1.PodCondition{}
}

func syncOnce(m *Manager, pod *corev1.Pod) {
	pod.Status = m.SyncPod(context.Background(), pod)
}

func TestInitContainersRunInOrderBeforeContainers(t *testing.T) {
	f, m, _ := newTestManager(t)
	pod := multiPod(corev1.RestartPolicyAlways)
	syncOnce(m, pod)
	st := pod.Status
	if st.InitContainerStatuses[0].State.Running == nil || waitingReason(st.InitContainerStatuses[1]) != ReasonPodInitializing ||
		waitingReason(st.ContainerStatuses[0]) != ReasonPodInitializing || st.Phase != corev1.PodPending {
		t.Fatalf("first init container must run alone: %+v", st)
	}
	if c := condition(st, corev1.PodInitialized); c.Status != corev1.ConditionFalse || c.Reason != "ContainersNotInitialized" ||
		c.Message != "containers with incomplete status: [a b]" {
		t.Fatalf("Initialized: %+v", c)
	}
	f.exitContainer("a", 0)
	syncOnce(m, pod)
	st = pod.Status
	if a := st.InitContainerStatuses[0]; a.State.Terminated == nil || a.State.Terminated.Reason != "Completed" || !a.Ready {
		t.Fatalf("completed init container: %+v", a)
	}
	if st.InitContainerStatuses[1].State.Running == nil || st.Phase != corev1.PodPending {
		t.Fatalf("second init container must run next: %+v", st)
	}
	f.exitContainer("b", 0)
	syncOnce(m, pod)
	st = pod.Status
	if st.Phase != corev1.PodRunning || st.ContainerStatuses[0].State.Running == nil || st.ContainerStatuses[1].State.Running == nil {
		t.Fatalf("containers must start after init: %+v", st)
	}
	for _, c := range []string{corev1.PodInitialized, corev1.ContainersReady, corev1.PodReady} {
		if condition(st, c).Status != corev1.ConditionTrue {
			t.Fatalf("%s: %+v", c, st.Conditions)
		}
	}
	// Completed init containers are not run again.
	syncOnce(m, pod)
	if _, c := f.count(); c != 4 {
		t.Fatalf("want 4 containers (2 init, 2 regular), got %d", c)
	}
}

func TestFailedInitContainerIsRetriedUnderAlways(t *testing.T) {
	f, m, now := newTestManager(t)
	pod := multiPod(corev1.RestartPolicyAlways)
	syncOnce(m, pod)
	f.exitContainer("a", 1)
	syncOnce(m, pod)
	a := pod.Status.InitContainerStatuses[0]
	if waitingReason(a) != ReasonCrashLoopBackOff || a.LastTerminationState.Terminated == nil || pod.Status.Phase != corev1.PodPending {
		t.Fatalf("failed init container must back off: %+v", pod.Status)
	}
	*now = now.Add(2 * time.Second)
	syncOnce(m, pod)
	if a := pod.Status.InitContainerStatuses[0]; a.State.Running == nil || a.RestartCount != 1 {
		t.Fatalf("init container must be retried: %+v", a)
	}
}

func TestFailedInitContainerFailsPodUnderNever(t *testing.T) {
	f, m, now := newTestManager(t)
	pod := multiPod(corev1.RestartPolicyNever)
	syncOnce(m, pod)
	f.exitContainer("a", 3)
	syncOnce(m, pod)
	st := pod.Status
	if a := st.InitContainerStatuses[0]; a.State.Terminated == nil || a.State.Terminated.ExitCode != 3 || a.State.Terminated.Reason != "Error" {
		t.Fatalf("init container: %+v", a)
	}
	if st.Phase != corev1.PodFailed || waitingReason(st.ContainerStatuses[0]) != ReasonPodInitializing {
		t.Fatalf("Pod must fail without starting containers: %+v", st)
	}
	if c := condition(st, corev1.PodReady); c.Reason != "PodFailed" {
		t.Fatalf("Ready: %+v", c)
	}
	// A finished Pod is left alone except for stopping its sandbox.
	*now = now.Add(time.Minute)
	before := st
	syncOnce(m, pod)
	if pod.Status.Phase != before.Phase || len(pod.Status.ContainerStatuses) != len(before.ContainerStatuses) {
		t.Fatalf("terminal status changed: %+v", pod.Status)
	}
	if _, c := f.count(); c != 1 {
		t.Fatalf("no container may start after the Pod failed: %d", c)
	}
	sandboxes, _ := m.sandboxes(context.Background(), pod.UID)
	if len(sandboxes) != 1 || sandboxes[0].State != runtimeapi.PodSandboxState_SANDBOX_NOTREADY {
		t.Fatalf("sandbox of a finished Pod must be stopped: %+v", sandboxes)
	}
}

func TestRestartPolicyNever(t *testing.T) {
	for name, tc := range map[string]struct {
		x, y  int32
		phase string
		ready string
	}{
		"all succeed": {0, 0, corev1.PodSucceeded, "PodCompleted"},
		"one fails":   {0, 2, corev1.PodFailed, "PodFailed"},
	} {
		f, m, now := newTestManager(t)
		pod := multiPod(corev1.RestartPolicyNever)
		pod.Spec.InitContainers = nil
		syncOnce(m, pod)
		f.exitContainer("x", tc.x)
		syncOnce(m, pod)
		if pod.Status.Phase != corev1.PodRunning || pod.Status.ContainerStatuses[0].State.Terminated == nil {
			t.Fatalf("%s: one container still runs: %+v", name, pod.Status)
		}
		if c := condition(pod.Status, corev1.PodReady); c.Message != "containers with unready status: [x]" {
			t.Fatalf("%s: Ready: %+v", name, c)
		}
		f.exitContainer("y", tc.y)
		*now = now.Add(time.Minute)
		syncOnce(m, pod)
		if pod.Status.Phase != tc.phase || condition(pod.Status, corev1.PodReady).Reason != tc.ready {
			t.Fatalf("%s: %+v", name, pod.Status)
		}
		if _, c := f.count(); c != 2 {
			t.Fatalf("%s: Never must not restart: %d containers", name, c)
		}
	}
}

func TestRestartPolicyOnFailure(t *testing.T) {
	f, m, now := newTestManager(t)
	pod := multiPod(corev1.RestartPolicyOnFailure)
	pod.Spec.InitContainers = nil
	syncOnce(m, pod)
	f.exitContainer("x", 0)
	f.exitContainer("y", 1)
	*now = now.Add(2 * time.Second)
	syncOnce(m, pod)
	x, y := pod.Status.ContainerStatuses[0], pod.Status.ContainerStatuses[1]
	if x.State.Terminated == nil || x.RestartCount != 0 {
		t.Fatalf("a container that succeeded is not restarted: %+v", x)
	}
	if y.State.Running == nil || y.RestartCount != 1 || pod.Status.Phase != corev1.PodRunning {
		t.Fatalf("a failed container is restarted: %+v", pod.Status)
	}
	f.exitContainer("y", 0)
	*now = now.Add(time.Minute)
	syncOnce(m, pod)
	if pod.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("all containers succeeded: %+v", pod.Status)
	}
}

func TestRestartPolicyAlwaysRestartsSucceededContainers(t *testing.T) {
	f, m, now := newTestManager(t)
	pod := multiPod(corev1.RestartPolicyAlways)
	pod.Spec.InitContainers = nil
	syncOnce(m, pod)
	f.exitContainer("x", 0)
	*now = now.Add(2 * time.Second)
	syncOnce(m, pod)
	if x := pod.Status.ContainerStatuses[0]; x.State.Running == nil || x.RestartCount != 1 || pod.Status.Phase != corev1.PodRunning {
		t.Fatalf("Always restarts even a successful exit: %+v", pod.Status)
	}
}

func TestKillPodStopsContainersInParallel(t *testing.T) {
	f, m, _ := newTestManager(t)
	pod := multiPod(corev1.RestartPolicyAlways)
	pod.Spec.InitContainers = nil
	syncOnce(m, pod)
	f.stopDelay = 300 * time.Millisecond
	start := time.Now()
	if err := m.KillPod(context.Background(), pod, 30); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 550*time.Millisecond {
		t.Fatalf("two containers stopped sequentially: %v", elapsed)
	}
	if len(f.stops) != 2 {
		t.Fatalf("stops: %v", f.stops)
	}
}

func TestExecRunsInTheRunningInstance(t *testing.T) {
	f, m, _ := newTestManager(t)
	pod := multiPod(corev1.RestartPolicyAlways)
	pod.Spec.InitContainers = nil
	syncOnce(m, pod)
	f.execExit = 7
	res, err := m.Exec(context.Background(), pod, "y", []string{"cat", "/etc/hostname"}, time.Minute)
	if err != nil || string(res.Stdout) != "out" || string(res.Stderr) != "err" || res.ExitCode != 7 {
		t.Fatalf("exec: %+v %v", res, err)
	}
	id := strings.TrimPrefix(pod.Status.ContainerStatuses[1].ContainerID, "containerd://")
	if len(f.execs) != 1 || f.execs[0][0] != id || f.execs[0][1] != "cat" {
		t.Fatalf("exec must target y's running instance %s: %v", id, f.execs)
	}
	f.exitContainer("y", 0)
	if _, err := m.Exec(context.Background(), pod, "y", []string{"true"}, time.Minute); err == nil {
		t.Fatal("exec into an exited container must fail")
	}
}

// loseSandbox simulates a node restart: sandboxes stop and containers exit.
func loseSandbox(f *fakeCRI) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, sb := range f.sandboxes {
		sb.state = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
	}
	for _, c := range f.containers {
		c.state = runtimeapi.ContainerState_CONTAINER_EXITED
	}
}

func TestNeverPodFailsInsteadOfRunningAgainWhenItsSandboxIsLost(t *testing.T) {
	f, m, now := newTestManager(t)
	pod := multiPod(corev1.RestartPolicyNever)
	pod.Spec.InitContainers = nil
	syncOnce(m, pod)
	f.exitContainer("x", 0)
	syncOnce(m, pod)
	loseSandbox(f)
	*now = now.Add(time.Minute)
	syncOnce(m, pod)
	x, y := pod.Status.ContainerStatuses[0], pod.Status.ContainerStatuses[1]
	if pod.Status.Phase != corev1.PodFailed || x.State.Terminated == nil || x.State.Terminated.Reason != "Completed" ||
		y.State.Terminated == nil || y.State.Terminated.Reason != "ContainerStatusUnknown" || y.State.Terminated.ExitCode != 137 {
		t.Fatalf("lost Never Pod: %+v", pod.Status)
	}
	if f.runs != 1 {
		t.Fatalf("no new sandbox may be created for a Never Pod that ran: %d", f.runs)
	}
}

func TestOnFailureKeepsSucceededContainersAcrossSandboxes(t *testing.T) {
	f, m, now := newTestManager(t)
	pod := multiPod(corev1.RestartPolicyOnFailure)
	syncOnce(m, pod)
	f.exitContainer("a", 0)
	syncOnce(m, pod)
	f.exitContainer("b", 0)
	syncOnce(m, pod)
	f.exitContainer("x", 0)
	syncOnce(m, pod)
	loseSandbox(f)
	*now = now.Add(time.Minute)
	for i := 0; i < 4; i++ { // the init containers run again, then y restarts
		syncOnce(m, pod)
		f.exitContainer("a", 0)
		f.exitContainer("b", 0)
	}
	x, y := pod.Status.ContainerStatuses[0], pod.Status.ContainerStatuses[1]
	if x.State.Terminated == nil || x.State.Terminated.ExitCode != 0 || y.State.Running == nil || pod.Status.Phase != corev1.PodRunning {
		t.Fatalf("x finished for good, y runs again: %+v", pod.Status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.cfg.Metadata.Name == "x" && c.sandbox != "sb1" {
			t.Fatalf("x was started again in the new sandbox")
		}
	}
}

func TestInitializedPodStaysInitializedWhenInitInstancesVanish(t *testing.T) {
	f, m, _ := newTestManager(t)
	pod := multiPod(corev1.RestartPolicyAlways)
	syncOnce(m, pod)
	f.exitContainer("a", 0)
	syncOnce(m, pod)
	f.exitContainer("b", 0)
	syncOnce(m, pod)
	if pod.Status.Phase != corev1.PodRunning {
		t.Fatalf("setup: %+v", pod.Status)
	}
	f.mu.Lock()
	for id, c := range f.containers {
		if c.cfg.Metadata.Name == "a" || c.cfg.Metadata.Name == "b" {
			delete(f.containers, id) // e.g. removed by hand with crictl rm
		}
	}
	f.mu.Unlock()
	syncOnce(m, pod)
	st := pod.Status
	if st.Phase != corev1.PodRunning || condition(st, corev1.PodInitialized).Status != corev1.ConditionTrue ||
		st.InitContainerStatuses[0].State.Terminated == nil || !st.ContainerStatuses[0].Ready {
		t.Fatalf("an initialized Pod must not go back to initializing: %+v", st)
	}
	if _, c := f.count(); c != 2 {
		t.Fatalf("init containers must not run again: %d containers", c)
	}
}

func TestCrashLoopBackOffStatusIsStableWhileWaiting(t *testing.T) {
	f, m, now := newTestManager(t)
	pod := testPod()
	syncOnce(m, pod)
	f.exit(1)
	syncOnce(m, pod)
	first := pod.Status.ContainerStatuses[0].State.Waiting
	*now = now.Add(600 * time.Millisecond)
	syncOnce(m, pod)
	second := pod.Status.ContainerStatuses[0].State.Waiting
	if first == nil || second == nil || first.Reason != ReasonCrashLoopBackOff || first.Message != second.Message ||
		!strings.HasPrefix(first.Message, "back-off 1s restarting failed container=app") {
		t.Fatalf("backoff message must not change while waiting: %+v then %+v", first, second)
	}
}

// fakeProbes reports scripted probe results.
type fakeProbes struct{ ready, started bool }

func (p *fakeProbes) Ready(_, _, _ string) bool   { return p.ready }
func (p *fakeProbes) Started(_, _, _ string) bool { return p.started }

func TestProbesGateReadyAndStarted(t *testing.T) {
	_, m, _ := newTestManager(t)
	probes := &fakeProbes{}
	m.Probes = probes
	pod := testPod()
	check := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
	pod.Spec.Containers[0].StartupProbe, pod.Spec.Containers[0].ReadinessProbe = check, check
	syncOnce(m, pod)
	cs := pod.Status.ContainerStatuses[0]
	if cs.State.Running == nil || cs.Ready || cs.Started == nil || *cs.Started || corev1.IsPodReady(pod) {
		t.Fatalf("running but neither started nor ready: %+v", cs)
	}
	probes.started = true
	syncOnce(m, pod)
	if cs := pod.Status.ContainerStatuses[0]; !*cs.Started || cs.Ready {
		t.Fatalf("started, not yet ready: %+v", cs)
	}
	probes.ready = true
	syncOnce(m, pod)
	if cs := pod.Status.ContainerStatuses[0]; !cs.Ready || !corev1.IsPodReady(pod) {
		t.Fatalf("ready: %+v", pod.Status)
	}
}

func TestContainersStoppedByProbesRestartEvenAfterExitingZero(t *testing.T) {
	for _, policy := range []string{corev1.RestartPolicyOnFailure, corev1.RestartPolicyNever} {
		f, m, now := newTestManager(t)
		pod := testPod()
		pod.Spec.RestartPolicy = policy
		syncOnce(m, pod)
		if err := m.StopContainer(context.Background(), pod, pod.Status.ContainerStatuses[0].ContainerID, 30); err != nil {
			t.Fatal(err)
		}
		f.exit(0) // the app exits cleanly on SIGTERM
		*now = now.Add(time.Minute)
		syncOnce(m, pod)
		cs := pod.Status.ContainerStatuses[0]
		if policy == corev1.RestartPolicyOnFailure && (cs.State.Running == nil || cs.RestartCount != 1) {
			t.Fatalf("OnFailure: a container killed by its liveness probe is restarted: %+v", cs)
		}
		if policy == corev1.RestartPolicyNever && (cs.State.Terminated == nil || pod.Status.Phase != corev1.PodSucceeded) {
			t.Fatalf("Never: nothing restarts: %+v", pod.Status)
		}
	}
}

func TestImagePullPolicies(t *testing.T) {
	f, m, now := newTestManager(t)
	pod := testPod()
	pod.Spec.Containers[0].Image, pod.Spec.Containers[0].ImagePullPolicy = "local:1", corev1.PullNever
	syncOnce(m, pod)
	if w := pod.Status.ContainerStatuses[0].State.Waiting; w == nil || w.Reason != ReasonErrImageNeverPull || f.pulls != 0 ||
		w.Message != `Container image "local:1" is not present with pull policy of Never` {
		t.Fatalf("Never must not pull: %+v pulls=%d", pod.Status.ContainerStatuses[0], f.pulls)
	}
	f.mu.Lock()
	f.images["local:1"] = true // e.g. kk image load
	f.mu.Unlock()
	syncOnce(m, pod) // no backoff: starts on the next sync
	if pod.Status.ContainerStatuses[0].State.Running == nil {
		t.Fatalf("Never with the image present: %+v", pod.Status.ContainerStatuses[0])
	}

	always := testPod()
	always.UID = "uid-2"
	always.Spec.Containers[0].ImagePullPolicy = corev1.PullAlways
	*now = now.Add(time.Minute)
	syncOnce(m, always)
	if f.pulls != 1 || always.Status.ContainerStatuses[0].State.Running == nil {
		t.Fatalf("Always pulls even a present image: pulls=%d", f.pulls)
	}
}
