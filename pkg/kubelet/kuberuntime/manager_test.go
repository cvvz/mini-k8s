package kuberuntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

// fakeCRI is an in-memory CRI runtime and image service.
type fakeCRI struct {
	runtimeapi.RuntimeServiceClient
	runtimeapi.ImageServiceClient

	mu         sync.Mutex
	seq        int
	sandboxes  map[string]*fakeSandbox
	containers map[string]*fakeContainer
	images     map[string]bool
	pullErr    error
	pulls      int
	sandboxErr error
	runs       int
	conditions []*runtimeapi.RuntimeCondition
	stops      []string
}

type fakeSandbox struct {
	cfg     *runtimeapi.PodSandboxConfig
	state   runtimeapi.PodSandboxState
	created int64
}

type fakeContainer struct {
	sandbox  string
	cfg      *runtimeapi.ContainerConfig
	state    runtimeapi.ContainerState
	exit     int32
	started  int64
	finished int64
	created  int64
}

func newFakeCRI() *fakeCRI {
	return &fakeCRI{sandboxes: map[string]*fakeSandbox{}, containers: map[string]*fakeContainer{},
		images:     map[string]bool{"img:1": true, "pause:1": true},
		conditions: []*runtimeapi.RuntimeCondition{{Type: "RuntimeReady", Status: true}, {Type: "NetworkReady", Status: true}}}
}

func (f *fakeCRI) id(prefix string) string { f.seq++; return fmt.Sprintf("%s%d", prefix, f.seq) }

func matches(labels, sel map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func (f *fakeCRI) Version(context.Context, *runtimeapi.VersionRequest, ...grpc.CallOption) (*runtimeapi.VersionResponse, error) {
	return &runtimeapi.VersionResponse{RuntimeName: "containerd", RuntimeVersion: "2.2.1"}, nil
}

func (f *fakeCRI) Status(context.Context, *runtimeapi.StatusRequest, ...grpc.CallOption) (*runtimeapi.StatusResponse, error) {
	return &runtimeapi.StatusResponse{Status: &runtimeapi.RuntimeStatus{Conditions: f.conditions}}, nil
}

func (f *fakeCRI) RunPodSandbox(_ context.Context, r *runtimeapi.RunPodSandboxRequest, _ ...grpc.CallOption) (*runtimeapi.RunPodSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	if f.sandboxErr != nil {
		return nil, fmt.Errorf("sandbox %s: %w", f.id("failed"), f.sandboxErr)
	}
	id := f.id("sb")
	f.sandboxes[id] = &fakeSandbox{cfg: r.Config, state: runtimeapi.PodSandboxState_SANDBOX_READY, created: int64(f.seq)}
	return &runtimeapi.RunPodSandboxResponse{PodSandboxId: id}, nil
}

func (f *fakeCRI) StopPodSandbox(_ context.Context, r *runtimeapi.StopPodSandboxRequest, _ ...grpc.CallOption) (*runtimeapi.StopPodSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sb := f.sandboxes[r.PodSandboxId]; sb != nil {
		sb.state = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
	}
	return &runtimeapi.StopPodSandboxResponse{}, nil
}

func (f *fakeCRI) RemovePodSandbox(_ context.Context, r *runtimeapi.RemovePodSandboxRequest, _ ...grpc.CallOption) (*runtimeapi.RemovePodSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sandboxes, r.PodSandboxId)
	for id, c := range f.containers {
		if c.sandbox == r.PodSandboxId {
			delete(f.containers, id)
		}
	}
	return &runtimeapi.RemovePodSandboxResponse{}, nil
}

func (f *fakeCRI) PodSandboxStatus(_ context.Context, r *runtimeapi.PodSandboxStatusRequest, _ ...grpc.CallOption) (*runtimeapi.PodSandboxStatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sandboxes[r.PodSandboxId] == nil {
		return nil, errors.New("not found")
	}
	return &runtimeapi.PodSandboxStatusResponse{Status: &runtimeapi.PodSandboxStatus{Id: r.PodSandboxId,
		Network: &runtimeapi.PodSandboxNetworkStatus{Ip: "10.244.1.5"}}}, nil
}

func (f *fakeCRI) ListPodSandbox(_ context.Context, r *runtimeapi.ListPodSandboxRequest, _ ...grpc.CallOption) (*runtimeapi.ListPodSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var items []*runtimeapi.PodSandbox
	for id, sb := range f.sandboxes {
		if r.Filter != nil && !matches(sb.cfg.Labels, r.Filter.LabelSelector) {
			continue
		}
		items = append(items, &runtimeapi.PodSandbox{Id: id, Metadata: sb.cfg.Metadata, State: sb.state, CreatedAt: sb.created, Labels: sb.cfg.Labels})
	}
	return &runtimeapi.ListPodSandboxResponse{Items: items}, nil
}

func (f *fakeCRI) CreateContainer(_ context.Context, r *runtimeapi.CreateContainerRequest, _ ...grpc.CallOption) (*runtimeapi.CreateContainerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.id("c")
	f.containers[id] = &fakeContainer{sandbox: r.PodSandboxId, cfg: r.Config, state: runtimeapi.ContainerState_CONTAINER_CREATED, created: int64(f.seq)}
	return &runtimeapi.CreateContainerResponse{ContainerId: id}, nil
}

func (f *fakeCRI) StartContainer(_ context.Context, r *runtimeapi.StartContainerRequest, _ ...grpc.CallOption) (*runtimeapi.StartContainerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[r.ContainerId]
	c.state, c.started = runtimeapi.ContainerState_CONTAINER_RUNNING, time.Now().UnixNano()
	return &runtimeapi.StartContainerResponse{}, nil
}

func (f *fakeCRI) StopContainer(_ context.Context, r *runtimeapi.StopContainerRequest, _ ...grpc.CallOption) (*runtimeapi.StopContainerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, fmt.Sprintf("%s/%d", r.ContainerId, r.Timeout))
	f.containers[r.ContainerId].state = runtimeapi.ContainerState_CONTAINER_EXITED
	return &runtimeapi.StopContainerResponse{}, nil
}

func (f *fakeCRI) RemoveContainer(_ context.Context, r *runtimeapi.RemoveContainerRequest, _ ...grpc.CallOption) (*runtimeapi.RemoveContainerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, r.ContainerId)
	return &runtimeapi.RemoveContainerResponse{}, nil
}

func (f *fakeCRI) ListContainers(_ context.Context, r *runtimeapi.ListContainersRequest, _ ...grpc.CallOption) (*runtimeapi.ListContainersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*runtimeapi.Container
	for id, c := range f.containers {
		if r.Filter != nil && ((r.Filter.PodSandboxId != "" && r.Filter.PodSandboxId != c.sandbox) || !matches(c.cfg.Labels, r.Filter.LabelSelector)) {
			continue
		}
		out = append(out, &runtimeapi.Container{Id: id, PodSandboxId: c.sandbox, Metadata: c.cfg.Metadata, State: c.state, CreatedAt: c.created, Labels: c.cfg.Labels})
	}
	return &runtimeapi.ListContainersResponse{Containers: out}, nil
}

func (f *fakeCRI) ContainerStatus(_ context.Context, r *runtimeapi.ContainerStatusRequest, _ ...grpc.CallOption) (*runtimeapi.ContainerStatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[r.ContainerId]
	if c == nil {
		return nil, errors.New("not found")
	}
	return &runtimeapi.ContainerStatusResponse{Status: &runtimeapi.ContainerStatus{Id: r.ContainerId, Metadata: c.cfg.Metadata,
		State: c.state, ExitCode: c.exit, StartedAt: c.started, FinishedAt: c.finished, ImageRef: "sha256:img"}}, nil
}

func (f *fakeCRI) ImageStatus(_ context.Context, r *runtimeapi.ImageStatusRequest, _ ...grpc.CallOption) (*runtimeapi.ImageStatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.images[r.Image.Image] {
		return &runtimeapi.ImageStatusResponse{Image: &runtimeapi.Image{Id: "sha256:" + r.Image.Image}}, nil
	}
	return &runtimeapi.ImageStatusResponse{}, nil
}

func (f *fakeCRI) PullImage(_ context.Context, r *runtimeapi.PullImageRequest, _ ...grpc.CallOption) (*runtimeapi.PullImageResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls++
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	f.images[r.Image.Image] = true
	return &runtimeapi.PullImageResponse{ImageRef: "sha256:" + r.Image.Image}, nil
}

// exit marks the running instance of the Pod's container as exited.
func (f *fakeCRI) exit(code int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.state == runtimeapi.ContainerState_CONTAINER_RUNNING {
			c.state, c.exit, c.finished = runtimeapi.ContainerState_CONTAINER_EXITED, code, time.Now().UnixNano()
		}
	}
}

func (f *fakeCRI) count() (sandboxes, containers int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sandboxes), len(f.containers)
}

func testPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-1"},
		Spec: corev1.PodSpec{NodeName: "worker-1", Containers: []corev1.Container{{
			Name: "app", Image: "img:1", Command: []string{"/app"}, Args: []string{"serve"}, Env: []corev1.EnvVar{{Name: "MODE", Value: "demo"}}}}},
	}
}

func newTestManager(t *testing.T) (*fakeCRI, *Manager, *time.Time) {
	f := newFakeCRI()
	m := NewManager(f, f)
	m.PodLogsDir = t.TempDir()
	now := time.Now()
	m.now = func() time.Time { return now }
	return f, m, &now
}

func TestSyncPodCreatesSandboxAndContainer(t *testing.T) {
	f, m, _ := newTestManager(t)
	pod := testPod()
	st := m.SyncPod(context.Background(), pod)
	cs := st.ContainerStatuses[0]
	if st.Phase != corev1.PodRunning || st.PodIP != "10.244.1.5" || cs.State.Running == nil || !cs.Ready || cs.RestartCount != 0 ||
		!strings.HasPrefix(cs.ContainerID, "containerd://") {
		t.Fatalf("status: %+v", st)
	}
	if c := st.Condition(corev1.PodReady); c == nil || c.Status != corev1.ConditionTrue {
		t.Fatalf("Ready condition: %+v", st.Conditions)
	}
	var sb *fakeSandbox
	var ctr *fakeContainer
	for _, s := range f.sandboxes {
		sb = s
	}
	for _, c := range f.containers {
		ctr = c
	}
	if sb.cfg.Hostname != "web" || sb.cfg.Labels[LabelPodUID] != "uid-1" || !strings.HasSuffix(sb.cfg.LogDirectory, "default_web_uid-1") {
		t.Fatalf("sandbox config: %+v", sb.cfg)
	}
	if ctr.cfg.LogPath != "app/0.log" || ctr.cfg.Labels[LabelContainerName] != "app" || ctr.cfg.Envs[0].Key != "MODE" || ctr.cfg.Command[0] != "/app" {
		t.Fatalf("container config: %+v", ctr.cfg)
	}
	pod.Status = st
	if again := m.SyncPod(context.Background(), pod); again.ContainerStatuses[0].ContainerID != cs.ContainerID {
		t.Fatal("steady state must not create anything")
	}
	if s, c := f.count(); s != 1 || c != 1 {
		t.Fatalf("sandboxes=%d containers=%d", s, c)
	}
}

func TestSyncPodRestartsWithBackoffAndKeepsPrevious(t *testing.T) {
	f, m, now := newTestManager(t)
	pod := testPod()
	pod.Status = m.SyncPod(context.Background(), pod)
	f.exit(3)
	*now = now.Add(2 * time.Second) // past the 1s window of the first start
	pod.Status = m.SyncPod(context.Background(), pod)
	cs := pod.Status.ContainerStatuses[0]
	if cs.RestartCount != 1 || cs.State.Running == nil || cs.LastTerminationState.Terminated == nil || cs.LastTerminationState.Terminated.ExitCode != 3 {
		t.Fatalf("first restart: %+v", cs)
	}
	f.exit(3)
	pod.Status = m.SyncPod(context.Background(), pod)
	cs = pod.Status.ContainerStatuses[0]
	if cs.State.Waiting == nil || cs.State.Waiting.Reason != ReasonCrashLoopBackOff || cs.Ready || pod.Status.Phase != corev1.PodRunning {
		t.Fatalf("must back off: %+v", cs)
	}
	touchLog(t, m, pod, "app", 0)
	touchLog(t, m, pod, "app", 1)
	*now = now.Add(3 * time.Second)
	pod.Status = m.SyncPod(context.Background(), pod)
	if pod.Status.ContainerStatuses[0].RestartCount != 2 {
		t.Fatalf("second restart: %+v", pod.Status.ContainerStatuses[0])
	}
	if _, c := f.count(); c != 2 {
		t.Fatalf("attempts 1 and 2 must remain, older removed: %d containers", c)
	}
	if attempts, _ := m.logAttempts(pod, "app"); len(attempts) != 1 || attempts[0] != 1 {
		t.Fatalf("only the previous instance's log may remain: %v", attempts)
	}
}

// touchLog creates the log file containerd would write for an instance.
func touchLog(t *testing.T, m *Manager, pod *corev1.Pod, name string, attempt int) string {
	t.Helper()
	dir := filepath.Join(m.PodLogDir(pod), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.log", attempt))
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSyncPodImagePullFailureAndBackoff(t *testing.T) {
	f, m, now := newTestManager(t)
	f.pullErr = errors.New("pull access denied")
	pod := testPod()
	pod.Spec.Containers[0].Image = "missing:1"
	st := m.SyncPod(context.Background(), pod)
	if w := st.ContainerStatuses[0].State.Waiting; w == nil || w.Reason != ReasonErrImagePull || !strings.Contains(w.Message, "pull access denied") || st.Phase != corev1.PodPending {
		t.Fatalf("first pull: %+v", st)
	}
	st = m.SyncPod(context.Background(), pod)
	if w := st.ContainerStatuses[0].State.Waiting; w.Reason != ReasonImagePullBackOff || f.pulls != 1 {
		t.Fatalf("backoff: pulls=%d %+v", f.pulls, st)
	}
	f.pullErr = nil
	*now = now.Add(5 * time.Second)
	if st = m.SyncPod(context.Background(), pod); st.Phase != corev1.PodRunning {
		t.Fatalf("after pull: %+v", st)
	}
}

func TestSyncPodBacksOffFailedSandboxCreation(t *testing.T) {
	f, m, now := newTestManager(t)
	f.sandboxErr = errors.New(`failed to find plugin "loopback"`)
	pod := testPod()
	first := m.SyncPod(context.Background(), pod)
	if w := first.ContainerStatuses[0].State.Waiting; w == nil || w.Reason != ReasonCreatePodSandboxError || !strings.Contains(w.Message, "loopback") {
		t.Fatalf("first attempt: %+v", first)
	}
	// Within the backoff window the runtime is not called again and the
	// status is identical, so the kubelet does not rewrite it in a loop.
	again := m.SyncPod(context.Background(), pod)
	if f.runs != 1 || again.ContainerStatuses[0].State.Waiting.Message != first.ContainerStatuses[0].State.Waiting.Message {
		t.Fatalf("retried during backoff: runs=%d %+v", f.runs, again)
	}
	*now = now.Add(2 * time.Second)
	m.SyncPod(context.Background(), pod)
	if f.runs != 2 {
		t.Fatalf("no retry after backoff: runs=%d", f.runs)
	}
	f.sandboxErr = nil
	*now = now.Add(5 * time.Second)
	if st := m.SyncPod(context.Background(), pod); st.Phase != corev1.PodRunning {
		t.Fatalf("after recovery: %+v", st)
	}
}

func TestSyncPodRecreatesSandboxAfterNodeRestart(t *testing.T) {
	f, m, _ := newTestManager(t)
	pod := testPod()
	pod.Status = m.SyncPod(context.Background(), pod)
	f.mu.Lock()
	for _, sb := range f.sandboxes {
		sb.state = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
	}
	for _, c := range f.containers {
		c.state = runtimeapi.ContainerState_CONTAINER_EXITED
	}
	f.mu.Unlock()
	pod.Status = m.SyncPod(context.Background(), pod)
	cs := pod.Status.ContainerStatuses[0]
	if cs.State.Running == nil || cs.RestartCount != 1 {
		t.Fatalf("restart count must continue across sandboxes: %+v", cs)
	}
	for _, sb := range f.sandboxes {
		if sb.cfg.Metadata.Attempt != 1 || sb.state != runtimeapi.PodSandboxState_SANDBOX_READY {
			t.Fatalf("new sandbox: %+v", sb.cfg.Metadata)
		}
	}
	if s, _ := f.count(); s != 1 {
		t.Fatalf("old sandbox not removed: %d", s)
	}
}

func TestKillPodAndCleanup(t *testing.T) {
	f, m, _ := newTestManager(t)
	pod := testPod()
	m.SyncPod(context.Background(), pod)
	other := testPod()
	other.UID, other.Name = "uid-2", "orphan"
	m.SyncPod(context.Background(), other)
	f.mu.Lock()
	f.sandboxes["manual"] = &fakeSandbox{cfg: &runtimeapi.PodSandboxConfig{Metadata: &runtimeapi.PodSandboxMetadata{}}, state: runtimeapi.PodSandboxState_SANDBOX_READY}
	f.mu.Unlock()
	kept := touchLog(t, m, pod, "app", 0)
	orphanLog := touchLog(t, m, other, "app", 0)
	gone := testPod()
	gone.UID, gone.Name = "uid-9", "deleted-while-node-was-down"
	staleLog := touchLog(t, m, gone, "app", 0)
	if err := m.Cleanup(context.Background(), map[string]bool{"uid-1": true}); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.count(); s != 2 {
		t.Fatalf("orphan must be removed, kept and unlabelled sandboxes kept: %d", s)
	}
	for path, want := range map[string]bool{kept: true, orphanLog: false, staleLog: false} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("log %s: exists=%v, want %v", path, err == nil, want)
		}
	}
	if err := m.KillPod(context.Background(), pod, 7); err != nil {
		t.Fatal(err)
	}
	if len(f.stops) != 1 || !strings.HasSuffix(f.stops[0], "/7") {
		t.Fatalf("stop must use the grace period: %v", f.stops)
	}
	if s, c := f.count(); s != 1 || c != 0 {
		t.Fatalf("killed Pod left sandboxes=%d containers=%d", s, c)
	}
	if _, err := os.Stat(m.PodLogDir(pod)); !os.IsNotExist(err) {
		t.Fatalf("killed Pod left its log directory: %v", err)
	}
}

// syncBuffer is a bytes.Buffer safe for one writer and concurrent readers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestLogsFollowStreamsAppendedLinesUntilExit(t *testing.T) {
	f, m, _ := newTestManager(t)
	pod := testPod()
	m.SyncPod(context.Background(), pod) // attempt 0 is running
	path := touchLog(t, m, pod, "app", 0)
	appendLog := func(text string) {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := file.WriteString(text); err != nil {
			t.Fatal(err)
		}
	}
	appendLog("2026-10-15T00:00:00Z stdout F before\n2026-10-15T00:00:01Z stdout F unfin")
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- m.Logs(context.Background(), pod, "app", LogOptions{Follow: true}, &out) }()
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for out.String() != want {
			if time.Now().After(deadline) {
				t.Fatalf("followed output %q, want %q", out.String(), want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("before\n")
	appendLog("ished\n2026-10-15T00:00:02Z stdout F after\n")
	waitFor("before\nunfinished\nafter\n")
	appendLog("2026-10-15T00:00:03Z stdout F last words\n")
	f.exit(0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not stop after the container exited")
	}
	if got := out.String(); got != "before\nunfinished\nafter\nlast words\n" {
		t.Fatalf("final output %q", got)
	}
}

func TestStatusReadiness(t *testing.T) {
	f, m, _ := newTestManager(t)
	if r := m.Status(context.Background(), "pause:1"); !r.Ready {
		t.Fatalf("ready runtime reported %+v", r)
	}
	if r := m.Status(context.Background(), "pause:2"); r.Ready || !strings.Contains(r.Message, "pause:2") {
		t.Fatalf("missing sandbox image: %+v", r)
	}
	f.conditions[1] = &runtimeapi.RuntimeCondition{Type: "NetworkReady", Reason: "NetworkPluginNotReady", Message: "cni plugin not initialized"}
	if r := m.Status(context.Background(), "pause:1"); r.Ready || !strings.Contains(r.Message, "NetworkReady=false") {
		t.Fatalf("network not ready: %+v", r)
	}
	if v, _ := m.Version(context.Background()); v != "containerd://2.2.1" {
		t.Fatal(v)
	}
}

func TestLogsTailPreviousAndPartialLines(t *testing.T) {
	_, m, _ := newTestManager(t)
	pod := testPod()
	dir := filepath.Join(m.PodLogDir(pod), "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("0.log", "2026-10-15T00:00:00Z stdout F first run\n")
	write("1.log", "2026-10-15T00:00:01Z stdout F one\n2026-10-15T00:00:02Z stderr P par\n2026-10-15T00:00:02Z stderr F tial\n2026-10-15T00:00:03Z stdout F three\n")
	var out bytes.Buffer
	if err := m.Logs(context.Background(), pod, "app", LogOptions{}, &out); err != nil || out.String() != "one\npartial\nthree\n" {
		t.Fatalf("logs %q %v", out.String(), err)
	}
	out.Reset()
	if err := m.Logs(context.Background(), pod, "app", LogOptions{TailLines: 1}, &out); err != nil || out.String() != "three\n" {
		t.Fatalf("tail %q %v", out.String(), err)
	}
	out.Reset()
	if err := m.Logs(context.Background(), pod, "app", LogOptions{Previous: true}, &out); err != nil || out.String() != "first run\n" {
		t.Fatalf("previous %q %v", out.String(), err)
	}
	if err := m.Logs(context.Background(), pod, "other", LogOptions{}, &out); err == nil {
		t.Fatal("unknown container must fail")
	}
}
