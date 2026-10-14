package nodeinit

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContainerdConfigPinsSandboxImage(t *testing.T) {
	cfg := ContainerdConfig("docker.io/library/mini-k8s-pause:local")
	for _, want := range []string{"version = 3", "[plugins.'io.containerd.cri.v1.images'.pinned_images]", "sandbox = 'docker.io/library/mini-k8s-pause:local'"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("missing %q in:\n%s", want, cfg)
		}
	}
}

func TestDelegateCgroups(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cgroup.controllers", "cpuset cpu io memory pids\n")
	write("cgroup.procs", "1\n42\n")
	write("cgroup.subtree_control", "")
	if err := DelegateCgroups(root); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
	if string(got) != "+cpuset +cpu +io +memory +pids" {
		t.Fatalf("subtree_control = %q", got)
	}
	if moved, _ := os.ReadFile(filepath.Join(root, "init.scope", "cgroup.procs")); string(moved) != "42" {
		t.Fatalf("processes not moved into init.scope: %q", moved)
	}
}

func TestDelegateCgroupsSkipsCgroupV1(t *testing.T) {
	if err := DelegateCgroups(t.TempDir()); err != nil {
		t.Fatalf("missing cgroup.controllers must be ignored: %v", err)
	}
}

func TestWaitForSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	go func() {
		time.Sleep(200 * time.Millisecond)
		l, err := net.Listen("unix", path)
		if err == nil {
			defer l.Close()
			c, _ := l.Accept()
			if c != nil {
				c.Close()
			}
		}
	}()
	if err := WaitForSocket(context.Background(), path, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := WaitForSocket(context.Background(), filepath.Join(t.TempDir(), "none.sock"), 300*time.Millisecond); err == nil {
		t.Fatal("missing socket reported ready")
	}
}
