package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
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

func TestClusterDownStopsNodesAndReconcilersFirst(t *testing.T) {
	for _, volumes := range []bool{false, true} {
		var calls [][]string
		c := cluster{
			run: func(_ context.Context, name string, args ...string) (string, error) {
				calls = append(calls, append([]string{name}, args...))
				return "", nil
			},
		}
		if err := c.down(context.Background(), volumes); err != nil {
			t.Fatal(err)
		}
		down := []string{"docker", "compose", "down", "--remove-orphans"}
		if volumes {
			down = append(down, "--volumes")
		}
		want := [][]string{
			{"docker", "compose", "stop", "worker-1", "worker-2"},
			down,
		}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("volumes=%v: unsafe down sequence: %#v", volumes, calls)
		}
	}
}

func TestLoadImagesPipesDockerSaveIntoEveryNodeAndRetries(t *testing.T) {
	var pipes [][]string
	failures := 1
	c := cluster{
		out: io.Discard,
		pipe: func(_ context.Context, from, to []string) (string, error) {
			pipes = append(pipes, append(append(append([]string{}, from...), "|"), to...))
			if failures > 0 {
				failures--
				return "", errors.New("containerd is starting")
			}
			return "", nil
		},
	}
	if err := c.loadImages(context.Background(), []string{"app:v1"}, workerNames...); err != nil {
		t.Fatal(err)
	}
	load := func(node string) []string {
		return []string{"docker", "save", "app:v1", "|", "docker", "exec", "-i", "mini-k8s-" + node + "-1", "ctr", "-n", "k8s.io", "images", "import", "-"}
	}
	want := [][]string{load("worker-1"), load("worker-1"), load("worker-2")}
	if !reflect.DeepEqual(pipes, want) {
		t.Fatalf("image load sequence: %#v", pipes)
	}
}

func TestCommandPipeConnectsProducerToConsumer(t *testing.T) {
	pipe := commandPipe(t.TempDir(), os.Environ())
	out, err := pipe(context.Background(), []string{"echo", "hello"}, []string{"tr", "a-z", "A-Z"})
	if err != nil || out != "HELLO\n" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := pipe(context.Background(), []string{"echo", "x"}, []string{"false"}); err == nil {
		t.Fatal("a failing consumer must fail the pipe")
	}
	if _, err := pipe(context.Background(), []string{"false"}, []string{"cat"}); err == nil {
		t.Fatal("a failing producer must fail the pipe")
	}
}

func TestClusterDownDoesNotRemoveWhenStopFails(t *testing.T) {
	calls := 0
	c := cluster{run: func(_ context.Context, name string, args ...string) (string, error) {
		calls++
		return "", errors.New("stop failed")
	}}
	if err := c.down(context.Background(), false); err == nil || !strings.Contains(err.Error(), "stop failed") || calls != 1 {
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

func TestReadinessRequiresAuthenticatedHealthAndAllFreshNodes(t *testing.T) {
	for _, scenario := range []string{"ready", "unauthorized", "stale", "one-worker", "wrong-worker", "no-control-plane"} {
		t.Run(scenario, func(t *testing.T) {
			token := "secret"
			if scenario == "unauthorized" {
				token = "other"
			}
			server := fakeNodeServer(t, token, func() []corev1.Node {
				nodes := []corev1.Node{readyNode("worker-1", time.Now()), readyNode("worker-2", time.Now()), readyNode("control-plane", time.Now())}
				switch scenario {
				case "stale":
					nodes[1] = readyNode("worker-2", time.Now().Add(-time.Minute))
				case "one-worker":
					nodes = append(nodes[:1], nodes[2])
				case "wrong-worker":
					nodes[1].Name = "other"
				case "no-control-plane":
					nodes = nodes[:2]
				}
				return nodes
			})
			c := cluster{client: client.New(server.URL, "secret")}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := c.waitReady(ctx, time.Now().Add(-10*time.Second))
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

func TestClusterUpRestartsStaticPodsRunningAnOldImage(t *testing.T) {
	var calls []string
	c := cluster{
		out: io.Discard,
		run: func(_ context.Context, name string, args ...string) (string, error) {
			cmd := strings.Join(append([]string{name}, args...), " ")
			calls = append(calls, cmd)
			switch {
			case strings.Contains(cmd, "crictl ps -o json"):
				return `{"containers": [
					{"id": "old", "imageRef": "sha256:1", "image": {"image": "docker.io/library/mini-k8s:local"}, "labels": {"io.kubernetes.pod.name": "kube-apiserver-control-plane"}},
					{"id": "fresh", "imageRef": "sha256:2", "image": {"image": "docker.io/library/mini-k8s:local"}, "labels": {}}]}`, nil
			case strings.Contains(cmd, "inspecti"):
				return "sha256:2\n", nil
			}
			return "", nil
		},
	}
	if err := c.restartStaleControlPlane(context.Background()); err != nil {
		t.Fatal(err)
	}
	var stops []string
	for _, cmd := range calls {
		if strings.Contains(cmd, "crictl stop") {
			stops = append(stops, cmd)
		}
	}
	if len(stops) != 1 || !strings.HasSuffix(stops[0], " old") {
		t.Fatalf("only the container on the old image restarts: %v", stops)
	}
}
