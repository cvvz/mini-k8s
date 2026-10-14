package kuberuntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// KillPod stops the Pod's containers (SIGTERM, then SIGKILL after grace
// seconds) and removes its sandboxes.
func (m *Manager) KillPod(ctx context.Context, pod *corev1.Pod, grace int64) error {
	sandboxes, err := m.sandboxes(ctx, pod.UID)
	if err != nil {
		return err
	}
	var errs []error
	for _, sb := range sandboxes {
		resp, err := m.runtime.ListContainers(ctx, &runtimeapi.ListContainersRequest{Filter: &runtimeapi.ContainerFilter{PodSandboxId: sb.Id}})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// Containers stop in parallel, so the whole Pod takes at most one
		// grace period.
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, c := range resp.Containers {
			if c.State != runtimeapi.ContainerState_CONTAINER_RUNNING {
				continue
			}
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				stopCtx, cancel := context.WithTimeout(ctx, time.Duration(grace)*time.Second+30*time.Second)
				defer cancel()
				if _, err := m.runtime.StopContainer(stopCtx, &runtimeapi.StopContainerRequest{ContainerId: id, Timeout: grace}); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}(c.Id)
		}
		wg.Wait()
		if err := m.removeSandbox(ctx, sb.Id); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		// containerd leaves log files behind; removing them is the kubelet's job.
		if err := os.RemoveAll(m.PodLogDir(pod)); err != nil {
			errs = append(errs, err)
		}
	}
	m.forget(pod.UID)
	return errors.Join(errs...)
}

func (m *Manager) forget(uid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sandboxRuns, uid)
	for key := range m.restarts {
		if strings.HasPrefix(key, uid+"/") {
			delete(m.restarts, key)
		}
	}
}

// StopContainer stops one container instance (given as its status ID,
// "containerd://...") within grace seconds, as after a failed liveness probe.
func (m *Manager) StopContainer(ctx context.Context, containerID string, grace int64) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(grace)*time.Second+30*time.Second)
	defer cancel()
	_, err := m.runtime.StopContainer(ctx, &runtimeapi.StopContainerRequest{
		ContainerId: strings.TrimPrefix(containerID, "containerd://"), Timeout: grace})
	return err
}

// Cleanup removes sandboxes and log directories of Pods that are not in keep
// (by UID). Sandboxes without Pod metadata (not created by a kubelet) are left
// alone.
func (m *Manager) Cleanup(ctx context.Context, keep map[string]bool) error {
	resp, err := m.runtime.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{})
	if err != nil {
		return err
	}
	var errs []error
	for _, sb := range resp.Items {
		uid := sb.Labels[LabelPodUID]
		if uid == "" || keep[uid] {
			continue
		}
		if err := m.removeSandbox(ctx, sb.Id); err != nil {
			errs = append(errs, err)
			continue
		}
		m.forget(uid)
	}
	// Log directories are named <namespace>_<name>_<uid>.
	entries, err := os.ReadDir(m.PodLogsDir)
	if err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	for _, e := range entries {
		i := strings.LastIndexByte(e.Name(), '_')
		if !e.IsDir() || i < 0 || keep[e.Name()[i+1:]] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(m.PodLogsDir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
