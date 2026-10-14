package prober

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/intstr"
)

func TestExecutorHTTPTCPAndExec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/moved":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	host, portText, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portText)
	c := corev1.Container{Name: "web", Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: int32(port)}}}
	exitCode := int32(0)
	e := Executor{Exec: func(_ context.Context, _ *corev1.Pod, container string, cmd []string, _ time.Duration) (*corev1.PodExecResult, error) {
		return &corev1.PodExecResult{Stdout: []byte("not ready yet\n"), ExitCode: exitCode}, nil
	}}
	httpProbe := func(path string, p intstr.IntOrString) *corev1.Probe {
		return &corev1.Probe{TimeoutSeconds: 1, ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: p, Scheme: "HTTP"}}}
	}
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	for name, tc := range map[string]struct {
		probe *corev1.Probe
		ok    bool
		msg   string
	}{
		"http ok":       {httpProbe("/ok", intstr.FromInt(port)), true, ""},
		"http redirect": {httpProbe("/moved", intstr.FromInt(port)), true, ""},
		"http 500":      {httpProbe("/fail", intstr.FromInt(port)), false, "HTTP probe failed with statuscode: 500"},
		"named port":    {httpProbe("/ok", intstr.FromString("http")), true, ""},
		"unknown name":  {httpProbe("/ok", intstr.FromString("metrics")), false, `couldn't find port "metrics" in container web`},
		"tcp open":      {&corev1.Probe{TimeoutSeconds: 1, ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(port)}}}, true, ""},
		"tcp closed":    {&corev1.Probe{TimeoutSeconds: 1, ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(closedPort)}}}, false, "connection refused"},
		"exec":          {&corev1.Probe{TimeoutSeconds: 1, ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"check"}}}}, true, ""},
	} {
		ok, msg := e.Run(context.Background(), &corev1.Pod{}, host, c, tc.probe)
		if ok != tc.ok || !strings.Contains(msg, tc.msg) {
			t.Errorf("%s: %v %q, want %v %q", name, ok, msg, tc.ok, tc.msg)
		}
	}
	exitCode = 1
	if ok, msg := e.Run(context.Background(), &corev1.Pod{}, host, c, &corev1.Probe{TimeoutSeconds: 1,
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"check"}}}}); ok || msg != "not ready yet" {
		t.Errorf("failing exec: %v %q", ok, msg)
	}
}

// fakeChecks returns scripted results per probe type and counts calls.
type fakeChecks struct {
	mu     sync.Mutex
	ok     map[ProbeType]bool
	calls  map[ProbeType]int
	events []string
}

func (f *fakeChecks) run(_ context.Context, _ *corev1.Pod, _ string, c corev1.Container, p *corev1.Probe) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := Readiness
	switch p {
	case c.LivenessProbe:
		t = Liveness
	case c.StartupProbe:
		t = Startup
	}
	f.calls[t]++
	return f.ok[t], "boom"
}

func (f *fakeChecks) Eventf(_ metav1.Object, _, reason, format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, reason+": "+fmt.Sprintf(format, args...))
}

func (f *fakeChecks) setOK(t ProbeType, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ok[t] = ok
}

func (f *fakeChecks) count(t ProbeType) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[t]
}

func newTestManager() (*Manager, *fakeChecks, chan string, chan string) {
	f := &fakeChecks{ok: map[ProbeType]bool{}, calls: map[ProbeType]int{}}
	m := NewManager(nil, f)
	m.Run, m.Second = f.run, 10*time.Millisecond
	changed, failed := make(chan string, 100), make(chan string, 100)
	m.Changed = func(p *corev1.Pod) { changed <- p.Name }
	m.Failed = func(_ *corev1.Pod, id, msg string) { failed <- id + " " + msg }
	return m, f, changed, failed
}

func probePod(c corev1.Container) (*corev1.Pod, corev1.PodStatus) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", UID: "uid"}, Spec: corev1.PodSpec{Containers: []corev1.Container{c}}}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}
	return pod, corev1.PodStatus{PodIP: "10.244.1.2", ContainerStatuses: []corev1.ContainerStatus{{Name: c.Name, ContainerID: "c1", State: running}}}
}

func probe(failures int32) *corev1.Probe {
	return &corev1.Probe{PeriodSeconds: 1, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: failures,
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"x"}}}}
}

func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReadinessFollowsThresholds(t *testing.T) {
	m, f, changed, _ := newTestManager()
	pod, status := probePod(corev1.Container{Name: "web", ReadinessProbe: probe(2)})
	defer m.Remove("uid")
	if m.Ready("uid", "web", "c1") {
		t.Fatal("ready before any probe")
	}
	f.setOK(Readiness, true)
	m.Sync(pod, status)
	<-changed
	if !m.Ready("uid", "web", "c1") || m.Ready("uid", "web", "c2") {
		t.Fatal("readiness belongs to the probed instance")
	}
	f.setOK(Readiness, false)
	<-changed
	if m.Ready("uid", "web", "c1") {
		t.Fatal("still ready after failureThreshold failures")
	}
	if n := f.count(Readiness); n < 3 {
		t.Fatalf("one success and two failures expected, got %d checks", n)
	}
}

func TestLivenessFailureIsReportedOnceAndStopsTheWorker(t *testing.T) {
	m, f, _, failed := newTestManager()
	pod, status := probePod(corev1.Container{Name: "web", LivenessProbe: probe(3)})
	defer m.Remove("uid")
	m.Sync(pod, status)
	if got := <-failed; got != "c1 Container web failed liveness probe, will be restarted" {
		t.Fatalf("failure: %s", got)
	}
	n := f.count(Liveness)
	time.Sleep(50 * time.Millisecond)
	if n != 3 || f.count(Liveness) != 3 {
		t.Fatalf("the worker must stop after reporting: %d then %d checks", n, f.count(Liveness))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) != 3 || f.events[0] != "Unhealthy: Liveness probe failed: boom" {
		t.Fatalf("events: %v", f.events)
	}
}

func TestStartupProbeHoldsOffOtherProbes(t *testing.T) {
	m, f, changed, _ := newTestManager()
	pod, status := probePod(corev1.Container{Name: "web", StartupProbe: probe(100), ReadinessProbe: probe(3), LivenessProbe: probe(1)})
	defer m.Remove("uid")
	f.setOK(Readiness, true)
	m.Sync(pod, status)
	eventually(t, func() bool { return f.count(Startup) >= 3 }, "startup checks")
	if f.count(Readiness) != 0 || f.count(Liveness) != 0 || m.Started("uid", "web", "c1") {
		t.Fatal("liveness and readiness must wait for the startup probe")
	}
	f.setOK(Startup, true)
	f.setOK(Liveness, true)
	<-changed // started
	eventually(t, func() bool { return m.Ready("uid", "web", "c1") }, "readiness after startup")
	n := f.count(Startup)
	time.Sleep(50 * time.Millisecond)
	if f.count(Startup) != n {
		t.Fatal("a started container is not probed for startup again")
	}
}

func TestSyncRestartsWorkersForNewInstancesAndStopsForExited(t *testing.T) {
	m, f, changed, _ := newTestManager()
	pod, status := probePod(corev1.Container{Name: "web", ReadinessProbe: probe(3)})
	defer m.Remove("uid")
	f.setOK(Readiness, true)
	m.Sync(pod, status)
	<-changed
	status.ContainerStatuses[0].ContainerID = "c2"
	m.Sync(pod, status)
	if m.Ready("uid", "web", "c2") {
		t.Fatal("a new instance starts unready")
	}
	<-changed
	if !m.Ready("uid", "web", "c2") {
		t.Fatal("new instance not probed")
	}
	status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
	m.Sync(pod, status)
	n := f.count(Readiness)
	time.Sleep(50 * time.Millisecond)
	if f.count(Readiness) != n {
		t.Fatal("exited container still probed")
	}
}
