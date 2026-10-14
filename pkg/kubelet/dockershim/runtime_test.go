package dockershim

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "mini-k8s/pkg/apis/meta/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
)

// fakeDocker emulates the Engine endpoints the runtime uses.
type fakeDocker struct {
	t          *testing.T
	mu         sync.Mutex
	images     map[string]bool
	pullErr    string
	containers map[string]*fakeContainer
	created    []map[string]any
	starts     int
	stops      []string
	removed    []string
	pulls      int
}

type fakeContainer struct {
	labels   map[string]string
	state    string
	exitCode int
	logs     string
}

func newFakeDocker(t *testing.T) (*fakeDocker, *Runtime) {
	f := &fakeDocker{t: t, images: map[string]bool{"img:1": true}, containers: map[string]*fakeContainer{}}
	s := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(s.Close)
	rt, err := NewRuntime("/unused", "test", "node-a", "mini-k8s")
	if err != nil {
		t.Fatal(err)
	}
	rt.docker.http, rt.docker.baseURL = s.Client(), s.URL
	return f, rt
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeDocker) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1.41")
	switch {
	case r.Method == "GET" && path == "/containers/json":
		var filters map[string][]string
		_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
		var out []any
		for id, c := range f.containers {
			match := true
			for _, l := range filters["label"] {
				k, v, _ := strings.Cut(l, "=")
				if c.labels[k] != v {
					match = false
				}
			}
			if match {
				out = append(out, map[string]any{"Id": id, "Labels": c.labels, "State": c.state})
			}
		}
		writeJSON(w, out)
	case r.Method == "GET" && strings.HasPrefix(path, "/images/"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		if !f.images[name] {
			w.WriteHeader(404)
			writeJSON(w, map[string]string{"message": "no such image"})
			return
		}
		writeJSON(w, map[string]string{"Id": "sha256:" + name})
	case r.Method == "POST" && path == "/images/create":
		f.pulls++
		if f.pullErr != "" {
			writeJSON(w, map[string]any{"errorDetail": map[string]string{"message": f.pullErr}})
			return
		}
		f.images[r.URL.Query().Get("fromImage")] = true
		writeJSON(w, map[string]string{"status": "pulled"})
	case r.Method == "POST" && path == "/containers/create":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.created = append(f.created, body)
		labels := map[string]string{}
		for k, v := range body["Labels"].(map[string]any) {
			labels[k] = v.(string)
		}
		id := "c" + string(rune('0'+len(f.created)))
		f.containers[id] = &fakeContainer{labels: labels, state: "created"}
		w.WriteHeader(201)
		writeJSON(w, map[string]string{"Id": id})
	case r.Method == "POST" && strings.HasSuffix(path, "/start"):
		f.starts++
		f.containers[strings.Split(path, "/")[2]].state = "running"
		w.WriteHeader(204)
	case r.Method == "POST" && strings.HasSuffix(path, "/stop"):
		id := strings.Split(path, "/")[2]
		f.stops = append(f.stops, id+"?t="+r.URL.Query().Get("t"))
		f.containers[id].state = "exited"
		w.WriteHeader(204)
	case r.Method == "DELETE" && strings.HasPrefix(path, "/containers/"):
		id := strings.TrimPrefix(path, "/containers/")
		f.removed = append(f.removed, id)
		delete(f.containers, id)
		w.WriteHeader(204)
	case r.Method == "GET" && strings.HasSuffix(path, "/logs"):
		c := f.containers[strings.Split(path, "/")[2]]
		var buf bytes.Buffer
		for _, line := range strings.SplitAfter(c.logs, "\n") {
			if line == "" {
				continue
			}
			hdr := [8]byte{1}
			binary.BigEndian.PutUint32(hdr[4:], uint32(len(line)))
			buf.Write(hdr[:])
			buf.WriteString(line)
		}
		_, _ = w.Write(buf.Bytes())
	case r.Method == "GET" && strings.HasSuffix(path, "/json"):
		id := strings.Split(path, "/")[2]
		c, ok := f.containers[id]
		if !ok {
			w.WriteHeader(404)
			writeJSON(w, map[string]string{"message": "no such container"})
			return
		}
		writeJSON(w, map[string]any{
			"Id": id, "Image": "sha256:img", "Config": map[string]any{"Labels": c.labels},
			"State": map[string]any{"Status": c.state, "Running": c.state == "running", "ExitCode": c.exitCode,
				"StartedAt": time.Now().UTC().Format(time.RFC3339Nano), "FinishedAt": time.Now().UTC().Format(time.RFC3339Nano)},
			"NetworkSettings": map[string]any{"Networks": map[string]any{"mini-k8s": map[string]string{"IPAddress": "10.244.0.9"}}},
		})
	default:
		f.t.Errorf("unexpected Docker call %s %s", r.Method, r.URL)
		w.WriteHeader(500)
	}
}

func testPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-1"},
		Spec: corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{
			Name: "app", Image: "img:1", Command: []string{"/app"}, Args: []string{"serve"}, Env: []corev1.EnvVar{{Name: "MODE", Value: "demo"}},
		}}},
	}
}

func TestSyncPodCreatesLabelledContainerAndReportsRunning(t *testing.T) {
	f, rt := newFakeDocker(t)
	st := rt.SyncPod(context.Background(), testPod())
	if len(f.created) != 1 || f.starts != 1 || f.pulls != 0 {
		t.Fatalf("created=%d starts=%d pulls=%d", len(f.created), f.starts, f.pulls)
	}
	body := f.created[0]
	wantLabels := map[string]any{labelCluster: "test", labelNode: "node-a", labelPodUID: "uid-1", labelPodName: "web", labelNamespace: "default", labelContainer: "app"}
	if !reflect.DeepEqual(body["Labels"], wantLabels) || !reflect.DeepEqual(body["Entrypoint"], []any{"/app"}) ||
		!reflect.DeepEqual(body["Cmd"], []any{"serve"}) || !reflect.DeepEqual(body["Env"], []any{"MODE=demo"}) || body["Hostname"] != "web" {
		t.Fatalf("create payload: %#v", body)
	}
	host := body["HostConfig"].(map[string]any)
	if host["NetworkMode"] != "mini-k8s" {
		t.Fatalf("HostConfig: %#v", host)
	}
	for _, forbidden := range []string{"Binds", "Privileged", "RestartPolicy"} {
		if _, ok := host[forbidden]; ok {
			t.Errorf("unwanted HostConfig field %s", forbidden)
		}
	}
	cs := st.ContainerStatuses[0]
	if st.Phase != corev1.PodRunning || st.PodIP != "10.244.0.9" || cs.State.Running == nil || !cs.Ready || cs.ContainerID != "docker://c1" {
		t.Fatalf("status: %+v", st)
	}
	if c := st.Condition(corev1.PodReady); c == nil || c.Status != corev1.ConditionTrue {
		t.Fatalf("Ready condition: %+v", st.Conditions)
	}
	again := testPod()
	again.Status = st
	if st2 := rt.SyncPod(context.Background(), again); !reflect.DeepEqual(st, st2) || len(f.created) != 1 {
		t.Fatalf("steady state must be idempotent:\n%+v\n%+v", st, st2)
	}
}

func TestSyncPodRestartsExitedContainerWithBackoff(t *testing.T) {
	f, rt := newFakeDocker(t)
	now := time.Now()
	rt.now = func() time.Time { return now }
	pod := testPod()
	pod.Status = rt.SyncPod(context.Background(), pod)
	f.containers["c1"].state, f.containers["c1"].exitCode = "exited", 3

	now = now.Add(2 * time.Second) // past the 1s window recorded by the initial start
	pod.Status = rt.SyncPod(context.Background(), pod)
	cs := pod.Status.ContainerStatuses[0]
	if f.starts != 2 || cs.RestartCount != 1 || cs.State.Running == nil || cs.LastTerminationState.Terminated.ExitCode != 3 {
		t.Fatalf("first restart: starts=%d %+v", f.starts, cs)
	}
	f.containers["c1"].state = "exited"
	pod.Status = rt.SyncPod(context.Background(), pod)
	cs = pod.Status.ContainerStatuses[0]
	if f.starts != 2 || cs.State.Waiting == nil || cs.State.Waiting.Reason != ReasonCrashLoopBackOff || pod.Status.Phase != corev1.PodRunning {
		t.Fatalf("must back off before the second restart: starts=%d %+v", f.starts, cs)
	}
	if c := pod.Status.Condition(corev1.PodReady); c.Status != corev1.ConditionFalse {
		t.Fatal("crash-looping Pod must not be Ready")
	}
	now = now.Add(3 * time.Second)
	pod.Status = rt.SyncPod(context.Background(), pod)
	if f.starts != 3 || pod.Status.ContainerStatuses[0].RestartCount != 2 {
		t.Fatalf("restart after backoff: starts=%d %+v", f.starts, pod.Status.ContainerStatuses[0])
	}
}

func TestSyncPodPullsMissingImageAndBacksOff(t *testing.T) {
	f, rt := newFakeDocker(t)
	f.pullErr = "pull access denied"
	pod := testPod()
	pod.Spec.Containers[0].Image = "missing:1"
	st := rt.SyncPod(context.Background(), pod)
	w := st.ContainerStatuses[0].State.Waiting
	if f.pulls != 1 || w == nil || w.Reason != ReasonErrImagePull || !strings.Contains(w.Message, "pull access denied") || st.Phase != corev1.PodPending {
		t.Fatalf("first pull: pulls=%d %+v", f.pulls, st)
	}
	st = rt.SyncPod(context.Background(), pod)
	if w := st.ContainerStatuses[0].State.Waiting; f.pulls != 1 || w.Reason != ReasonImagePullBackOff {
		t.Fatalf("second attempt must back off: pulls=%d %+v", f.pulls, st)
	}
	f.pullErr = ""
	rt.pulls = map[string]backoff{}
	if st = rt.SyncPod(context.Background(), pod); st.Phase != corev1.PodRunning || f.pulls != 2 {
		t.Fatalf("pull after backoff: %+v", st)
	}
}

func TestKillPodStopsWithGraceAndCleanupRespectsOwnership(t *testing.T) {
	f, rt := newFakeDocker(t)
	pod := testPod()
	rt.SyncPod(context.Background(), pod)
	f.containers["foreign"] = &fakeContainer{labels: map[string]string{labelCluster: "test", labelNode: "node-b", labelPodUID: "x"}, state: "running"}
	f.containers["orphan"] = &fakeContainer{labels: map[string]string{labelCluster: "test", labelNode: "node-a", labelPodUID: "gone"}, state: "exited"}

	if err := rt.Cleanup(context.Background(), map[string]bool{"uid-1": true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.containers["orphan"]; ok {
		t.Fatal("orphan on this node must be removed")
	}
	if _, ok := f.containers["foreign"]; !ok {
		t.Fatal("containers of other nodes must never be touched")
	}
	if err := rt.KillPod(context.Background(), pod, 7); err != nil {
		t.Fatal(err)
	}
	if len(f.stops) != 1 || f.stops[0] != "c1?t=7" {
		t.Fatalf("stop must use the grace period: %v", f.stops)
	}
	if _, ok := f.containers["c1"]; ok {
		t.Fatal("killed container must be removed")
	}
}

func TestLogsDemultiplexes(t *testing.T) {
	f, rt := newFakeDocker(t)
	pod := testPod()
	rt.SyncPod(context.Background(), pod)
	f.containers["c1"].logs = "hello\nworld\n"
	var out bytes.Buffer
	if err := rt.Logs(context.Background(), pod, "app", false, 0, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello\nworld\n" {
		t.Fatalf("logs: %q", out.String())
	}
	if err := rt.Logs(context.Background(), pod, "other", false, 0, &out); err == nil {
		t.Fatal("unknown container must fail")
	}
}

func TestNewRuntimeRejectsUnsafeConfiguration(t *testing.T) {
	for _, tc := range []struct{ cluster, node, network string }{
		{"", "n", "mini-k8s"}, {"c", "", "mini-k8s"}, {"c", "n", "host"}, {"c", "n", "bridge"}, {"c", "n", "container:x"},
	} {
		if _, err := NewRuntime("/s", tc.cluster, tc.node, tc.network); err == nil {
			t.Errorf("%+v accepted", tc)
		}
	}
}

func TestPodHostnameIsValid(t *testing.T) {
	long := strings.Repeat("a", 62) + "-b" + strings.Repeat("c", 20)
	for in, want := range map[string]string{"web-abc": "web-abc", long: strings.Repeat("a", 62), "x.y.": "x.y"} {
		if got := podHostname(in); got != want || len(got) > 63 {
			t.Errorf("podHostname(%q) = %q, want %q", in, got, want)
		}
	}
}
