package framework

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/onsi/gomega/gbytes"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// Workers are the worker nodes; Nodes adds the control-plane node, which runs
// the control plane as static Pods.
var (
	Workers = []string{"worker-1", "worker-2"}
	Nodes   = append([]string{"control-plane"}, Workers...)
)

// RunKK runs the project's kk binary from the repository root and returns its
// combined output.
func RunKK(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, TestContext.KKPath, args...)
	cmd.Dir = TestContext.RepoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("kk %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// KK runs kk and fails the spec on error.
func KK(ctx context.Context, args ...string) string {
	out, err := RunKK(ctx, args...)
	ExpectNoError(err)
	return out
}

// Docker runs the docker CLI against the local engine.
func Docker(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// Crictl runs crictl against the containerd inside a node container.
func Crictl(ctx context.Context, node string, args ...string) (string, error) {
	return Docker(ctx, append([]string{"exec", ComponentContainer(node), "crictl"}, args...)...)
}

// PodContainers lists the CRI container IDs of a Pod UID across all nodes.
func PodContainers(ctx context.Context, podUID string, runningOnly bool) ([]string, error) {
	args := []string{"ps", "-q", "--label", "io.kubernetes.pod.uid=" + podUID}
	if !runningOnly {
		args = append(args, "-a")
	}
	return acrossNodes(ctx, args)
}

// PodSandboxes lists the CRI sandbox IDs matching a label across all nodes.
func PodSandboxes(ctx context.Context, label string) ([]string, error) {
	return acrossNodes(ctx, []string{"pods", "-q", "--label", label})
}

func acrossNodes(ctx context.Context, crictlArgs []string) ([]string, error) {
	var ids []string
	for _, node := range Nodes {
		out, err := Crictl(ctx, node, crictlArgs...)
		if err != nil {
			return nil, err
		}
		ids = append(ids, strings.Fields(out)...)
	}
	return ids, nil
}

// ExecInPod runs a command in the Pod's first container, like kubectl exec.
func ExecInPod(ctx context.Context, pod corev1.Pod, command ...string) (string, error) {
	return Crictl(ctx, pod.Spec.NodeName, append([]string{"exec", ContainerID(pod)}, command...)...)
}

// ComponentContainer returns the Compose container name of a service, e.g.
// "apiserver" -> "mini-k8s-apiserver-1", "worker-1" -> "mini-k8s-worker-1-1".
func ComponentContainer(component string) string {
	return "mini-k8s-" + component + "-1"
}

// PodLogDirs lists a Pod UID's log directories (/var/log/pods/<ns>_<name>_<uid>) across all nodes.
func PodLogDirs(ctx context.Context, podUID string) ([]string, error) {
	var dirs []string
	for _, node := range Nodes {
		out, err := Docker(ctx, "exec", ComponentContainer(node), "sh", "-c", "ls /var/log/pods 2>/dev/null || true")
		if err != nil {
			return nil, err
		}
		for _, name := range strings.Fields(out) {
			if strings.HasSuffix(name, "_"+podUID) {
				dirs = append(dirs, node+":"+name)
			}
		}
	}
	return dirs, nil
}

// NodeZombies counts zombie processes in a node container.
func NodeZombies(ctx context.Context, node string) (int, error) {
	out, err := Docker(ctx, "exec", ComponentContainer(node), "ps", "-eo", "stat=")
	zombies := 0
	for _, stat := range strings.Fields(out) {
		if strings.HasPrefix(stat, "Z") {
			zombies++
		}
	}
	return zombies, err
}

// StartKK runs kk in the background, streaming its combined output into the
// returned buffer until stop is called (for kk get -w and kk logs -f).
func StartKK(ctx context.Context, args ...string) (out *gbytes.Buffer, stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	out = gbytes.NewBuffer()
	cmd := exec.CommandContext(ctx, TestContext.KKPath, args...)
	cmd.Dir = TestContext.RepoRoot
	cmd.Stdout, cmd.Stderr = out, out
	ExpectNoError(cmd.Start())
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return out, func() {
		cancel()
		<-done
	}
}

// FetchFromPod GETs url from inside a Pod of the spec namespace with
// `kk exec POD -- /mini fetch URL`; out is the status code line and body.
func (f *Framework) FetchFromPod(ctx context.Context, pod, url string) (string, error) {
	return RunKK(ctx, "exec", "-n", f.Namespace, pod, "--", "/mini", "fetch", url)
}
