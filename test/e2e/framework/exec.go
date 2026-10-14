package framework

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
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

// PodContainers lists Docker container IDs owned by the given Pod UID.
func PodContainers(ctx context.Context, podUID string, runningOnly bool) ([]string, error) {
	args := []string{"ps", "-q", "--filter", "label=mini.cluster=mini-k8s", "--filter", "label=mini.pod=" + podUID}
	if !runningOnly {
		args = append(args, "-a")
	}
	out, err := Docker(ctx, args...)
	return strings.Fields(out), err
}

// ComponentContainer returns the Compose container name for a control-plane
// component, e.g. "apiserver" -> "mini-k8s-apiserver-1".
func ComponentContainer(component string) string {
	return "mini-k8s-" + component + "-1"
}
