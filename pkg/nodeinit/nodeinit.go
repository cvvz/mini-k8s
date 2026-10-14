// Package nodeinit prepares a kind-style node container and runs its daemons:
// it delegates cgroup v2 controllers, makes mounts shared, enables IP
// forwarding, writes the containerd configuration, starts containerd and then
// runs the kubelet. If either exits, the node exits and is restarted.
package nodeinit

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Options configures a node.
type Options struct {
	// SandboxImage is the pause image containerd uses for Pod sandboxes.
	SandboxImage string
	// CgroupRoot is the cgroup v2 mount point, normally /sys/fs/cgroup.
	CgroupRoot string
	// ContainerdConfig is where the containerd configuration is written.
	ContainerdConfig string
	// ContainerdSocket is where containerd serves CRI.
	ContainerdSocket string
	// Kubelet runs the node agent until ctx ends.
	Kubelet func(ctx context.Context) error
}

// Defaults for a node container.
const (
	DefaultSandboxImage     = "docker.io/library/mini-k8s-pause:local"
	DefaultContainerdConfig = "/etc/containerd/config.toml"
	DefaultContainerdSocket = "/run/containerd/containerd.sock"
)

// ContainerdConfig renders the containerd configuration. Only the sandbox
// image differs from containerd's defaults (CNI binaries in /opt/cni/bin,
// configuration in /etc/cni/net.d).
func ContainerdConfig(sandboxImage string) string {
	return fmt.Sprintf(`version = 3

[plugins.'io.containerd.cri.v1.images'.pinned_images]
  sandbox = '%s'
`, sandboxImage)
}

// DelegateCgroups enables every cgroup v2 controller for child cgroups. The
// "no internal processes" rule forbids that while processes live in the root
// cgroup of the container's namespace, so they are moved into init.scope
// first (what kind's entrypoint does). cgroup v1 hosts are left untouched.
func DelegateCgroups(root string) error {
	controllers, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	scope := filepath.Join(root, "init.scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		return err
	}
	procs, err := os.ReadFile(filepath.Join(root, "cgroup.procs"))
	if err != nil {
		return err
	}
	for _, pid := range strings.Fields(string(procs)) {
		// Kernel threads and exited processes cannot move; that is fine.
		_ = os.WriteFile(filepath.Join(scope, "cgroup.procs"), []byte(pid), 0o644)
	}
	return os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte(enableAll(string(controllers))), 0o644)
}

// enableAll turns "cpu io memory" into "+cpu +io +memory".
func enableAll(controllers string) string {
	fields := strings.Fields(controllers)
	for i, f := range fields {
		fields[i] = "+" + f
	}
	return strings.Join(fields, " ")
}

// Run prepares the node and runs containerd and the kubelet until ctx ends or
// one of them exits.
func Run(ctx context.Context, o Options) error {
	if err := DelegateCgroups(o.CgroupRoot); err != nil {
		return fmt.Errorf("delegate cgroups: %w", err)
	}
	if err := makeMountsShared(); err != nil {
		return fmt.Errorf("make / rshared: %w", err)
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(o.ContainerdConfig), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(o.ContainerdConfig, []byte(ContainerdConfig(o.SandboxImage)), 0o644); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	containerd := exec.CommandContext(ctx, "containerd", "--config", o.ContainerdConfig, "--log-level", "warn")
	containerd.Stdout, containerd.Stderr = os.Stdout, os.Stderr
	if err := containerd.Start(); err != nil {
		return fmt.Errorf("start containerd: %w", err)
	}
	exited := make(chan error, 2)
	go func() { exited <- fmt.Errorf("containerd exited: %w", containerd.Wait()) }()
	if err := WaitForSocket(ctx, o.ContainerdSocket, 30*time.Second); err != nil {
		return err
	}
	log.Printf("node-init: containerd is serving on %s", o.ContainerdSocket)
	go func() { exited <- fmt.Errorf("kubelet exited: %w", o.Kubelet(ctx)) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-exited:
		return err
	}
}

// WaitForSocket waits until a Unix socket accepts connections.
func WaitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			return conn.Close()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not ready after %s: %w", path, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
