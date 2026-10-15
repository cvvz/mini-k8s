package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
)

// Scaling from three workers to two: worker-3 (already gone from the API,
// so not drained) loses its container and disk; other volumes stay.
func TestClusterUpBuildsLocalImagesWithoutComposePull(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".mini-k8s"), 0o700); err != nil {
		t.Fatal(err)
	}
	server := fakeNodeServer(t, "test-token", func() []corev1.Node {
		return []corev1.Node{readyNode("control-plane", time.Now()), readyNode("worker-1", time.Now()), readyNode("worker-2", time.Now())}
	})
	var calls [][]string
	c := cluster{
		client: client.New(server.URL, "test-token"),
		out:    io.Discard,
		run: func(_ context.Context, name string, args ...string) (string, error) {
			calls = append(calls, append([]string{name}, args...))
			if len(args) > 1 && args[0] == "volume" && args[1] == "ls" {
				return "mini-k8s_control-plane-var\nmini-k8s_worker-2-var\nmini-k8s_worker-3-var\nmini-k8s_worker-3-data\n", nil
			}
			return "", nil
		},
		pipe: func(_ context.Context, from, to []string) (string, error) {
			calls = append(calls, append(append(append([]string{}, from...), "|"), to...))
			return "", nil
		},
		root:    root,
		workers: 3,
	}
	if err := c.up(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	load := func(node string, extra ...string) []string {
		return append(append(append([]string{"docker", "save", "mini-k8s:local", "mini-k8s-pause:local", "mini-k8s-kube-proxy:local"}, extra...), "|",
			"docker", "exec", "-i", "mini-k8s-"+node+"-1"), "ctr", "-n", "k8s.io", "images", "import", "-")
	}
	ps := func(node string) []string {
		return []string{"docker", "exec", "mini-k8s-" + node + "-1", "crictl", "ps", "-o", "json", "--label", "io.kubernetes.pod.namespace=kube-system"}
	}
	want := [][]string{
		{"make", "build", "prepare-etcd"},
		{"docker", "build", "-t", "mini-k8s:local", "-f", "build/images/mini/Dockerfile", "."},
		{"docker", "build", "-t", "mini-k8s-pause:local", "-f", "build/images/pause/Dockerfile", "."},
		{"docker", "build", "-t", "mini-k8s-node:local", "-f", "build/images/node/Dockerfile", "."},
		{"docker", "build", "-t", "mini-k8s-kube-proxy:local", "-f", "build/images/kube-proxy/Dockerfile", "."},
		{"docker", "build", "-t", "mini-k8s-etcd:3.5.17", "-f", "build/images/etcd/Dockerfile", "."},
		{"docker", "compose", "-f", "compose.yaml", "-f", ".mini-k8s/compose.workers.yaml", "up", "-d", "--pull", "never", "--remove-orphans"},
		{"docker", "volume", "ls", "-q", "--filter", "label=com.docker.compose.project=mini-k8s"},
		{"docker", "volume", "rm", "--force", "mini-k8s_worker-3-var"},
		load("control-plane", "mini-k8s-etcd:3.5.17"),
		ps("control-plane"),
		load("worker-1"),
		load("worker-2"),
		ps("worker-1"),
		ps("worker-2"),
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("startup must build every image, never pull, and preload node images: %#v", calls)
	}
	if n, err := loadWorkers(root); err != nil || n != 2 {
		t.Fatalf("the new worker count must be recorded: %d %v", n, err)
	}
}

func TestPrepareEtcdDownloadsExactMemberThenReusesCache(t *testing.T) {
	root := t.TempDir()
	makefile, err := filepath.Abs("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(root, "fixture.tar.gz")
	file, err := os.Create(fixture)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range map[string]string{
		"etcd-v3.5.17-linux-arm64/etcd":      "test-etcd-binary",
		"etcd-v3.5.17-linux-arm64/unrelated": "must-not-extract",
	} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Size: int64(len(content)), Mode: 0755}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tarWriter, content); err != nil {
			t.Fatal(err)
		}
	}
	for _, closer := range []io.Closer{tarWriter, gzipWriter, file} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	tools := filepath.Join(root, "tools")
	if err := os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
if [ "$FAIL_DOWNLOAD" = 1 ]; then exit 23; fi
printf '%s\n' "$@" > "$CURL_ARGS"
output=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ] || [ "$1" = "--output" ]; then
    shift
    output=$1
  fi
  shift
done
cp "$FIXTURE" "$output"
`
	if err := os.WriteFile(filepath.Join(tools, "curl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(root, "curl-args")
	run := func(fail string) {
		t.Helper()
		cmd := exec.Command("make", "-f", makefile, "prepare-etcd", "GOARCH=arm64")
		cmd.Dir = root
		cmd.Env = withEnv(os.Environ(), "PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
		cmd.Env = append(cmd.Env, "FIXTURE="+fixture, "CURL_ARGS="+argsFile, "FAIL_DOWNLOAD="+fail)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("prepare-etcd: %v\n%s", err, output)
		}
	}
	run("0")
	data, err := os.ReadFile(filepath.Join(root, "bin", "etcd"))
	if err != nil || string(data) != "test-etcd-binary" {
		t.Fatalf("wrong extracted binary: %q %v", data, err)
	}
	info, err := os.Stat(filepath.Join(root, "bin", "etcd"))
	if err != nil || info.Mode().Perm()&0111 == 0 {
		t.Fatal("cached binary is not executable")
	}
	args, err := os.ReadFile(argsFile)
	if err != nil || !strings.Contains(string(args), "--max-time\n90\n") || !strings.Contains(string(args), "https://storage.googleapis.com/etcd/v3.5.17/etcd-v3.5.17-linux-arm64.tar.gz") {
		t.Fatalf("download must use official host and bounded timeout: %s (%v)", args, err)
	}
	run("1")
	entries, err := os.ReadDir(filepath.Join(root, "bin"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "etcd" {
		t.Fatalf("extracted extra files or leaked temporary downloads: %v %v", entries, err)
	}
}
