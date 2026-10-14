package mini

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func dockerTestPod() Resource {
	return Resource{
		APIVersion: "v1", Kind: "Pod", Metadata: Metadata{Name: "demo", UID: "pod-1"},
		Spec:   json.RawMessage(`{"containers":[{"name":"app","image":"mini-k8s:local","command":["/app"],"args":["serve"],"env":[{"name":"MODE","value":"demo"}]}]}`),
		Status: Status{NodeName: "node-a", Restarts: 2},
	}
}

func dockerTestLabels(pod string) map[string]string {
	return map[string]string{"mini.cluster": "test", "mini.node": "node-a", "mini.pod": pod}
}

func dockerTestEngine(t *testing.T, handler http.HandlerFunc) *Docker {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	d := NewDocker("/unused", "test", "node-a", "mini-k8s")
	d.client = s.Client()
	d.baseURL = s.URL
	return d
}

func dockerTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func dockerTestInspect(w http.ResponseWriter, labels map[string]string, state string) {
	dockerTestJSON(w, map[string]any{
		"Id":     "container-1",
		"Config": map[string]any{"Labels": labels, "Tty": false},
		"State":  map[string]any{"Status": state, "Running": state == "running", "ExitCode": 137},
		"NetworkSettings": map[string]any{"Networks": map[string]any{
			"bridge":   map[string]any{"IPAddress": "172.17.0.2"},
			"mini-k8s": map[string]any{"IPAddress": "172.30.0.3"},
		}},
	})
}

func TestDockerEnsureCreate(t *testing.T) {
	var created map[string]any
	starts, pulls := 0, 0
	d := dockerTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1.41/containers/json":
			if r.URL.Query().Get("all") != "1" {
				t.Error("list must include stopped containers")
			}
			var filters map[string][]string
			if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
				t.Error(err)
			}
			for _, label := range []string{"mini.cluster=test", "mini.node=node-a", "mini.pod=pod-1"} {
				if !strings.Contains(strings.Join(filters["label"], ","), label) {
					t.Errorf("missing filter %s", label)
				}
			}
			dockerTestJSON(w, []any{})
		case "GET /v1.41/images/mini-k8s:local/json":
			dockerTestJSON(w, map[string]string{"Id": "image-1"})
		case "POST /v1.41/images/create":
			pulls++
			t.Error("cached image must not be pulled")
		case "POST /v1.41/containers/create":
			if r.URL.Query().Get("name") != "mini-pod-1" {
				t.Errorf("wrong container name: %s", r.URL.RawQuery)
			}
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusCreated)
			dockerTestJSON(w, map[string]string{"Id": "container-1"})
		case "POST /v1.41/containers/container-1/start":
			starts++
			w.WriteHeader(http.StatusNoContent)
		case "GET /v1.41/containers/container-1/json":
			dockerTestInspect(w, dockerTestLabels("pod-1"), "running")
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected request", 500)
		}
	})
	status, err := d.Ensure(context.Background(), dockerTestPod())
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != "Running" || status.PodIP != "172.30.0.3" || status.ContainerID != "container-1" ||
		status.NodeName != "node-a" || status.Restarts != 2 || !status.Heartbeat.IsZero() {
		t.Fatalf("wrong status: %+v", status)
	}
	if starts != 1 || pulls != 0 {
		t.Fatalf("starts=%d pulls=%d", starts, pulls)
	}
	wantLabels := map[string]any{"mini.cluster": "test", "mini.node": "node-a", "mini.pod": "pod-1"}
	if !reflect.DeepEqual(created["Labels"], wantLabels) ||
		!reflect.DeepEqual(created["Entrypoint"], []any{"/app"}) ||
		!reflect.DeepEqual(created["Cmd"], []any{"serve"}) ||
		!reflect.DeepEqual(created["Env"], []any{"MODE=demo"}) || created["Image"] != "mini-k8s:local" {
		t.Fatalf("incorrect create payload: %#v", created)
	}
	host, ok := created["HostConfig"].(map[string]any)
	if !ok || host["NetworkMode"] != "mini-k8s" {
		t.Fatalf("wrong HostConfig: %#v", created["HostConfig"])
	}
	for _, forbidden := range []string{"Binds", "Privileged", "RestartPolicy"} {
		if _, exists := host[forbidden]; exists {
			t.Errorf("unsafe/unwanted HostConfig field %s", forbidden)
		}
	}
}

func TestDockerEnsureReuseAndBackoff(t *testing.T) {
	state := "exited"
	starts := 0
	d := dockerTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1.41/containers/json":
			dockerTestJSON(w, []any{map[string]any{"Id": "container-1", "Labels": dockerTestLabels("pod-1")}})
		case "GET /v1.41/containers/container-1/json":
			dockerTestInspect(w, dockerTestLabels("pod-1"), state)
		case "POST /v1.41/containers/container-1/start":
			starts++
			state = "running"
			w.WriteHeader(204)
		default:
			t.Errorf("reuse must not create/pull: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected request", 500)
		}
	})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	p := dockerTestPod()
	first, err := d.Ensure(context.Background(), p)
	if err != nil || first.Phase != "Running" || first.Restarts != 3 || starts != 1 {
		t.Fatalf("killed container did not recover: %+v err=%v starts=%d", first, err, starts)
	}
	p.Status = first
	reused, err := d.Ensure(context.Background(), p)
	if err != nil || reused.Restarts != 3 || starts != 1 {
		t.Fatalf("running container was restarted: %+v %v", reused, err)
	}
	for i := 0; i < 8; i++ {
		state = "exited"
		waiting, err := d.Ensure(context.Background(), p)
		if err != nil || waiting.Phase != "Pending" || !strings.Contains(waiting.Reason, "BackOff") || starts != i+1 {
			t.Fatalf("backoff failed at %d: %+v %v starts=%d", i, waiting, err, starts)
		}
		delay := time.Second << i
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		now = now.Add(delay - time.Millisecond)
		_, err = d.Ensure(context.Background(), p)
		if err != nil || starts != i+1 {
			t.Fatalf("restart before backoff deadline: %v starts=%d", err, starts)
		}
		now = now.Add(time.Millisecond)
		p.Status, err = d.Ensure(context.Background(), p)
		if err != nil || p.Status.Phase != "Running" || starts != i+2 || p.Status.Restarts != i+4 {
			t.Fatalf("restart after backoff: %+v %v starts=%d", p.Status, err, starts)
		}
	}
}

func TestDockerEnsureRejectsUnassignedAndInvalidPods(t *testing.T) {
	d := dockerTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid pod must not reach engine: %s", r.URL)
		http.Error(w, "unexpected", 500)
	})
	for _, change := range []func(*Resource){
		func(p *Resource) { p.Status.NodeName = "other" },
		func(p *Resource) { p.Status.NodeName = "" },
		func(p *Resource) { p.Kind = "Node" },
		func(p *Resource) { p.Metadata.UID = "" },
		func(p *Resource) { p.Metadata.UID = "../foreign" },
		func(p *Resource) { p.Spec = json.RawMessage(`{"containers":[]}`) },
	} {
		p := dockerTestPod()
		change(&p)
		status, err := d.Ensure(context.Background(), p)
		if err == nil || status.Phase != "Failed" || status.Reason == "" || status.NodeName != p.Status.NodeName {
			t.Errorf("invalid pod accepted: %+v err=%v", status, err)
		}
	}
}

func TestDockerCleanupOwnership(t *testing.T) {
	var removed []string
	items := []map[string]any{
		{"Id": "active", "Labels": dockerTestLabels("pod-1")},
		{"Id": "stopped-orphan", "Labels": dockerTestLabels("orphan")},
		{"Id": "assigned-elsewhere", "Labels": dockerTestLabels("pod-2")},
		{"Id": "no-pod", "Labels": dockerTestLabels("")},
		{"Id": "foreign-cluster", "Labels": map[string]string{"mini.cluster": "other", "mini.node": "node-a", "mini.pod": "x"}},
		{"Id": "foreign-node", "Labels": map[string]string{"mini.cluster": "test", "mini.node": "node-b", "mini.pod": "x"}},
		{"Id": "unlabelled"},
	}
	d := dockerTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1.41/containers/json" {
			if r.URL.Query().Get("all") != "1" {
				t.Error("cleanup must include stopped containers")
			}
			dockerTestJSON(w, items)
			return
		}
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/json") {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.41/containers/"), "/json")
			for _, item := range items {
				if item["Id"] == id {
					dockerTestJSON(w, map[string]any{"Id": id, "Config": map[string]any{"Labels": item["Labels"]}})
					return
				}
			}
		}
		if r.Method == "DELETE" {
			removed = append(removed, strings.TrimPrefix(r.URL.Path, "/v1.41/containers/"))
			if r.URL.Query().Get("force") != "1" {
				t.Error("orphan removal must work for running containers")
			}
			w.WriteHeader(204)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", 500)
	})
	other := dockerTestPod()
	other.Metadata.UID = "pod-2"
	other.Status.NodeName = "node-b"
	if err := d.Cleanup(context.Background(), []Resource{dockerTestPod(), other}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, []string{"stopped-orphan", "assigned-elsewhere"}) {
		t.Fatalf("wrong removals: %v", removed)
	}
}

func dockerTestFrame(stream byte, text string) []byte {
	frame := make([]byte, 8+len(text))
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(text)))
	copy(frame[8:], text)
	return frame
}

type dockerFailWriter struct{}

func (dockerFailWriter) Write([]byte) (int, error) { return 0, errors.New("writer failed") }

func TestDockerLogs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      []byte
		labels    map[string]string
		code      int
		writer    io.Writer
		wantError string
	}{
		{name: "demultiplex", body: append(dockerTestFrame(1, "out\n"), dockerTestFrame(2, "err\n")...)},
		{name: "foreign", labels: map[string]string{"mini.cluster": "other", "mini.node": "node-a", "mini.pod": "pod-1"}, wantError: "owned"},
		{name: "missing pod label", labels: dockerTestLabels(""), wantError: "owned"},
		{name: "http error", code: 500, body: []byte(`{"message":"engine unavailable"}`), wantError: "engine unavailable"},
		{name: "truncated header", body: []byte{1, 0}, wantError: "header"},
		{name: "truncated payload", body: dockerTestFrame(1, "hello")[:10], wantError: "payload"},
		{name: "invalid stream", body: dockerTestFrame(9, "bad"), wantError: "stream"},
		{name: "writer error", body: dockerTestFrame(1, "hello"), writer: dockerFailWriter{}, wantError: "writer failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logCalls := 0
			d := dockerTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1.41/containers/container-1/json":
					labels := tc.labels
					if labels == nil {
						labels = dockerTestLabels("pod-1")
					}
					dockerTestInspect(w, labels, "running")
				case "/v1.41/containers/container-1/logs":
					logCalls++
					if r.URL.Query().Get("stdout") != "1" || r.URL.Query().Get("stderr") != "1" {
						t.Error("must request both streams")
					}
					if tc.code != 0 {
						w.WriteHeader(tc.code)
					}
					_, _ = w.Write(tc.body)
				default:
					t.Errorf("unexpected URL: %s", r.URL)
					http.Error(w, "unexpected", 500)
				}
			})
			var output bytes.Buffer
			writer := tc.writer
			if writer == nil {
				writer = &output
			}
			err := d.Logs(context.Background(), "container-1", writer)
			if tc.wantError == "" {
				if err != nil || output.String() != "out\nerr\n" {
					t.Fatalf("logs=%q err=%v", output.String(), err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("want error containing %q, got %v", tc.wantError, err)
			}
			if tc.labels != nil && logCalls != 0 {
				t.Fatal("accessed logs before verifying ownership")
			}
		})
	}
}

func TestDockerUnixSocketTransport(t *testing.T) {
	// A short path avoids macOS's Unix-domain socket path length limit.
	dir, err := os.MkdirTemp("/tmp", "mini-docker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(dir); err != nil {
			t.Error(err)
		}
	})
	socket := filepath.Join(dir, "engine.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.41/containers/json" {
			t.Errorf("wrong route: %s", r.URL)
		}
		dockerTestJSON(w, []any{})
	}))
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	d := NewDocker("unix://"+socket, "test", "node-a", "mini-k8s")
	t.Cleanup(d.client.CloseIdleConnections)
	if err := d.Cleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestDockerPullMissingImageAndBackoff(t *testing.T) {
	pulls, creates := 0, 0
	pullFails := true
	d := dockerTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1.41/containers/json":
			dockerTestJSON(w, []any{})
		case "GET /v1.41/images/library/alpine:3/json":
			http.Error(w, `{"message":"No such image"}`, 404)
		case "POST /v1.41/images/create":
			pulls++
			if r.URL.Query().Get("fromImage") != "library/alpine:3" {
				t.Errorf("wrong image: %s", r.URL)
			}
			if pullFails {
				_, _ = io.WriteString(w, "{\"status\":\"pulling\"}\n{\"errorDetail\":{\"message\":\"registry denied\"},\"error\":\"registry denied\"}\n")
			} else {
				_, _ = io.WriteString(w, "{\"status\":\"download complete\"}\n")
			}
		case "POST /v1.41/containers/create":
			creates++
			w.WriteHeader(201)
			dockerTestJSON(w, map[string]string{"Id": "container-1"})
		case "POST /v1.41/containers/container-1/start":
			w.WriteHeader(204)
		case "GET /v1.41/containers/container-1/json":
			dockerTestInspect(w, dockerTestLabels("pod-1"), "running")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	p := dockerTestPod()
	p.Spec = json.RawMessage(`{"containers":[{"name":"app","image":"library/alpine:3"}]}`)
	status, err := d.Ensure(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "registry denied") || status.Reason == "" || creates != 0 || pulls != 1 {
		t.Fatalf("pull stream failure lost: %+v %v creates=%d pulls=%d", status, err, creates, pulls)
	}
	_, err = d.Ensure(context.Background(), p)
	if err == nil || pulls != 1 {
		t.Fatalf("failed image pulled in tight loop: %v pulls=%d", err, pulls)
	}
	now = now.Add(30 * time.Second)
	pullFails = false
	status, err = d.Ensure(context.Background(), p)
	if err != nil || status.Phase != "Running" || pulls != 2 || creates != 1 {
		t.Fatalf("pull recovery failed: %+v %v creates=%d pulls=%d", status, err, creates, pulls)
	}
}

func TestDockerHTTPAndInspectFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		listCode    int
		listBody    string
		inspectBody string
		want        string
	}{
		{name: "list HTTP", listCode: 503, listBody: `{"message":"engine down"}`, want: "engine down"},
		{name: "list JSON", listBody: `{`, want: "decode"},
		{name: "inspect JSON", inspectBody: `{`, want: "decode"},
		{name: "inspect foreign", inspectBody: `{"Id":"container-1","Config":{"Labels":{"mini.cluster":"other","mini.node":"node-a","mini.pod":"pod-1"}}}`, want: "owned"},
		{name: "missing network", inspectBody: `{"Id":"container-1","Config":{"Labels":{"mini.cluster":"test","mini.node":"node-a","mini.pod":"pod-1"}},"State":{"Status":"running","Running":true},"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}}`, want: "network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dockerTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1.41/containers/json" {
					if tc.listCode != 0 {
						w.WriteHeader(tc.listCode)
					}
					if tc.listBody != "" {
						_, _ = io.WriteString(w, tc.listBody)
					} else {
						dockerTestJSON(w, []any{map[string]any{"Id": "container-1", "Labels": dockerTestLabels("pod-1")}})
					}
					return
				}
				if r.URL.Path == "/v1.41/containers/container-1/json" {
					_, _ = io.WriteString(w, tc.inspectBody)
					return
				}
				t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				http.Error(w, "unexpected", 500)
			})
			status, err := d.Ensure(context.Background(), dockerTestPod())
			if err == nil || !strings.Contains(err.Error(), tc.want) || status.Reason == "" ||
				status.NodeName != "node-a" || status.Phase == "Running" {
				t.Fatalf("want %q failure: status=%+v err=%v", tc.want, status, err)
			}
		})
	}
}

func TestDockerStartFailureBackoff(t *testing.T) {
	starts := 0
	d := dockerTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1.41/containers/json":
			dockerTestJSON(w, []any{map[string]any{"Id": "container-1", "Labels": dockerTestLabels("pod-1")}})
		case "GET /v1.41/containers/container-1/json":
			dockerTestInspect(w, dockerTestLabels("pod-1"), "exited")
		case "POST /v1.41/containers/container-1/start":
			starts++
			http.Error(w, `{"message":"start denied"}`, 500)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	p := dockerTestPod()
	status, err := d.Ensure(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "start denied") || status.Phase != "Failed" || status.Restarts != 2 {
		t.Fatalf("start failure lost: %+v %v", status, err)
	}
	status, err = d.Ensure(context.Background(), p)
	if err != nil || status.Phase != "Pending" || starts != 1 || status.Restarts != 2 {
		t.Fatalf("start failure must back off without counting a restart: %+v %v starts=%d", status, err, starts)
	}
}

func TestDockerCleanupReportsFailuresAndContinues(t *testing.T) {
	var removed []string
	d := dockerTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.41/containers/json" {
			dockerTestJSON(w, []any{
				map[string]any{"Id": "denied", "Labels": dockerTestLabels("orphan-1")},
				map[string]any{"Id": "gone", "Labels": dockerTestLabels("orphan-2")},
				map[string]any{"Id": "removable", "Labels": dockerTestLabels("orphan-3")},
			})
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.41/containers/"), "/json")
		if r.Method == "GET" {
			if id == "gone" {
				http.Error(w, `{"message":"already removed"}`, 404)
				return
			}
			pod := "orphan-1"
			if id == "removable" {
				pod = "orphan-3"
			}
			dockerTestJSON(w, map[string]any{"Id": id, "Config": map[string]any{"Labels": dockerTestLabels(pod)}})
			return
		}
		if r.Method == "DELETE" {
			removed = append(removed, id)
			if id == "denied" {
				http.Error(w, `{"message":"remove denied"}`, 500)
			} else {
				w.WriteHeader(204)
			}
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", 500)
	})
	err := d.Cleanup(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "remove denied") ||
		!reflect.DeepEqual(removed, []string{"denied", "removable"}) {
		t.Fatalf("cleanup failures not reported/continued: %v removed=%v", err, removed)
	}
}

func TestDockerContextCancellation(t *testing.T) {
	d := dockerTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	status, err := d.Ensure(ctx, dockerTestPod())
	if !errors.Is(err, context.DeadlineExceeded) || status.Phase != "Failed" || status.Reason == "" {
		t.Fatalf("request cancellation was lost: %+v %v", status, err)
	}
}

func TestDockerRejectsUnsafeConfiguration(t *testing.T) {
	for _, network := range []string{"host", "none", "bridge", "default", "container:foreign"} {
		t.Run(network, func(t *testing.T) {
			d := dockerTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unsafe network must not reach engine: %s", r.URL)
				http.Error(w, "unexpected", 500)
			})
			d.network = network
			if _, err := d.Ensure(context.Background(), dockerTestPod()); err == nil {
				t.Fatal("unsafe network accepted")
			}
		})
	}
}
