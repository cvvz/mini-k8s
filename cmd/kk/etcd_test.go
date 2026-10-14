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

	"mini-k8s/internal/api"
	"mini-k8s/internal/client"
)

func TestClusterUpBuildsLocalImagesWithoutComposePull(t *testing.T) {
	server := fakeNodeServer(t, "test-token", func() []api.Node {
		return []api.Node{readyNode("worker-1", time.Now()), readyNode("worker-2", time.Now())}
	})
	var calls [][]string
	c := cluster{
		client: client.New(server.URL, "test-token"),
		out:    io.Discard,
		run: func(_ context.Context, name string, args ...string) (string, error) {
			calls = append(calls, append([]string{name}, args...))
			return "", nil
		},
	}
	if err := c.up(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"make", "build", "prepare-etcd"},
		{"docker", "build", "-t", "mini-k8s:local", "."},
		{"docker", "build", "-t", "mini-k8s-etcd:3.5.17", "-f", "Dockerfile.etcd", "."},
		{"docker", "compose", "up", "-d", "--pull", "never"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("startup must build both images and never pull: %#v", calls)
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
