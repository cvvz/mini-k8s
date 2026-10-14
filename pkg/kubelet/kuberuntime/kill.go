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
		// containerd leaves log files and volumes behind; removing them is
		// the kubelet's job.
		if err := os.RemoveAll(m.PodLogDir(pod)); err != nil {
			errs = append(errs, err)
		}
		if err := m.removePodDir(pod.UID); err != nil {
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
	for id, owner := range m.probeKilled {
		if owner == uid {
			delete(m.probeKilled, id)
		}
	}
	for key := range m.restarts {
		if strings.HasPrefix(key, uid+"/") {
			delete(m.restarts, key)
		}
	}
}

// StopContainer stops a container instance of the Pod (given as its status
// ID, "containerd://...") within grace seconds after a failed liveness or
// startup probe. Unless the restart policy is Never, the instance is then
// replaced whatever its exit code, as in Kubernetes.
func (m *Manager) StopContainer(ctx context.Context, pod *corev1.Pod, containerID string, grace int64) error {
	id := strings.TrimPrefix(containerID, "containerd://")
	m.mu.Lock()
	m.probeKilled[id] = pod.UID
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(grace)*time.Second+30*time.Second)
	defer cancel()
	_, err := m.runtime.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: id, Timeout: grace})
	return err
}

// killedByProbe reports whether the instance was stopped after a failed probe.
func (m *Manager) killedByProbe(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.probeKilled[id]
	return ok
}

// Cleanup removes the sandboxes, volumes and log directories of Pods that
// keep does not return (by UID). Sandboxes without Pod metadata (not created
// by a kubelet) are left alone.
//
// Everything is listed before keep is called: a Pod whose sandbox or
// directory exists at listing time is either in the later keep set or really
// gone, so a Pod that arrives meanwhile never loses its data.
func (m *Manager) Cleanup(ctx context.Context, keep func() map[string]bool) error {
	resp, err := m.runtime.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{})
	if err != nil {
		return err
	}
	var errs []error
	// Pod directories are named by UID, log directories <namespace>_<name>_<uid>.
	podDirs, err := os.ReadDir(m.PodsDir)
	if err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	logDirs, err := os.ReadDir(m.PodLogsDir)
	if err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	kept := keep()
	for _, sb := range resp.Items {
		uid := sb.Labels[LabelPodUID]
		if uid == "" || kept[uid] {
			continue
		}
		if err := m.removeSandbox(ctx, sb.Id); err != nil {
			errs = append(errs, err)
			continue
		}
		m.forget(uid)
	}
	for _, e := range podDirs {
		if e.IsDir() && !kept[e.Name()] {
			if err := m.removePodDir(e.Name()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for _, e := range logDirs {
		i := strings.LastIndexByte(e.Name(), '_')
		if !e.IsDir() || i < 0 || kept[e.Name()[i+1:]] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(m.PodLogsDir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
