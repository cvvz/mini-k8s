package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mini-k8s/internal/mini"
)

func TestTokenCreationAndReuse(t *testing.T) {
	root := t.TempDir()
	first, err := loadToken(root, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadToken(root, true)
	if err != nil || first != second || len(first) != 64 {
		t.Fatalf("token not stable: len=%d err=%v", len(first), err)
	}
	info, err := os.Stat(filepath.Join(root, ".mini-k8s", "token"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("token permission: %v %v", info, err)
	}
	if _, err := loadToken(t.TempDir(), false); err == nil {
		t.Fatal("read-only token lookup created a token")
	}
	if err := os.WriteFile(filepath.Join(root, ".mini-k8s", "token"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(root, true); err == nil {
		t.Fatal("empty existing token must not be replaced silently")
	}
}

func TestDockerEndpointOnlyLocalUnix(t *testing.T) {
	for _, endpoint := range []string{"tcp://127.0.0.1:2375", "ssh://host", "unix://host/path", "unix:relative", "http://localhost", "unix:///tmp/a.sock?remote=true"} {
		if _, err := localSocket(endpoint); err == nil {
			t.Errorf("accepted unsafe endpoint %q", endpoint)
		}
	}
	if got, err := localSocket("unix:///Users/me/.docker/run/docker.sock"); err != nil || got != "/Users/me/.docker/run/docker.sock" {
		t.Fatalf("local socket: %q %v", got, err)
	}
}

func TestClusterDownStopsReconcilersBeforeRemovingWorkloads(t *testing.T) {
	var calls [][]string
	c := cluster{
		run: func(_ context.Context, name string, args ...string) (string, error) {
			calls = append(calls, append([]string{name}, args...))
			if len(args) > 0 && args[0] == "ps" {
				return "abc123\ndef456\n", nil
			}
			return "", nil
		},
	}
	if err := c.down(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"docker", "compose", "stop", "worker-1", "worker-2", "controller", "scheduler"},
		{"docker", "ps", "-aq", "--filter", "label=mini.cluster=mini-k8s", "--filter", "label=mini.pod"},
		{"docker", "rm", "-f", "abc123", "def456"},
		{"docker", "compose", "down"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("unsafe down sequence: %#v", calls)
	}
}

func TestClusterDownDoesNotRemoveWhenStopFails(t *testing.T) {
	calls := 0
	c := cluster{run: func(_ context.Context, name string, args ...string) (string, error) {
		calls++
		return "", errors.New("stop failed")
	}}
	if err := c.down(context.Background()); err == nil || !strings.Contains(err.Error(), "stop failed") || calls != 1 {
		t.Fatalf("continued after failed stop: calls=%d err=%v", calls, err)
	}
}

func TestProjectRootRejectsUnrelatedComposeProject(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("name: unrelated\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module other-project\n\ngo 1.23\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if isProject(root) {
		t.Fatal("unrelated Go/Compose project must not be used for cluster operations")
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module mini-k8s\n\ngo 1.23\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if !isProject(root) {
		t.Fatal("mini-k8s root was not recognized")
	}
}

func TestReadinessRequiresAuthenticatedHealthAndBothFreshWorkers(t *testing.T) {
	for _, scenario := range []string{"ready", "unauthorized", "stale", "one-worker", "wrong-worker"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer secret" || scenario == "unauthorized" {
					http.Error(w, "unauthorized", 401)
					return
				}
				if r.URL.Path == "/healthz" {
					_, _ = w.Write([]byte("{}"))
					return
				}
				nodes := []mini.Resource{
					{Kind: "Node", Metadata: mini.Metadata{Name: "worker-1"}, Status: mini.Status{Phase: "Ready", Heartbeat: time.Now()}},
					{Kind: "Node", Metadata: mini.Metadata{Name: "worker-2"}, Status: mini.Status{Phase: "Ready", Heartbeat: time.Now()}},
				}
				switch scenario {
				case "stale":
					nodes[1].Status.Heartbeat = time.Now().Add(-time.Minute)
				case "one-worker":
					nodes = nodes[:1]
				case "wrong-worker":
					nodes[1].Metadata.Name = "other"
				}
				_ = json.NewEncoder(w).Encode(nodes)
			}))
			defer server.Close()
			c := cluster{client: mini.NewClient(server.URL, "secret")}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := c.waitReady(ctx)
			if scenario == "ready" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("unready cluster did not fail clearly: %v", err)
			}
		})
	}
}
