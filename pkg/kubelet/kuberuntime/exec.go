package kuberuntime

import (
	"context"
	"fmt"
	"time"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// Exec runs cmd to completion in the running instance of a container (CRI
// ExecSync) and returns its output and exit code. A command that exits
// non-zero is a result, not an error.
func (m *Manager) Exec(ctx context.Context, pod *corev1.Pod, container string, cmd []string, timeout time.Duration) (*corev1.PodExecResult, error) {
	instances, err := m.containers(ctx, pod.UID, container)
	if err != nil {
		return nil, err
	}
	if len(instances) == 0 || instances[0].State != runtimeapi.ContainerState_CONTAINER_RUNNING {
		return nil, fmt.Errorf("container %q of pod %s/%s is not running", container, pod.Namespace, pod.Name)
	}
	resp, err := m.runtime.ExecSync(ctx, &runtimeapi.ExecSyncRequest{
		ContainerId: instances[0].Id, Cmd: cmd, Timeout: int64(timeout / time.Second)})
	if err != nil {
		return nil, err
	}
	return &corev1.PodExecResult{Stdout: resp.Stdout, Stderr: resp.Stderr, ExitCode: resp.ExitCode}, nil
}
