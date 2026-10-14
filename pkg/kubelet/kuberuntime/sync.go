package kuberuntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

// SyncPod drives the Pod towards running and returns the observed status.
// Runtime failures are reported in the status, never hidden. An exited
// container is replaced by a new instance (attempt+1) after an exponential
// backoff of 1s doubling to 30s; the previous instance is kept for logs.
func (m *Manager) SyncPod(ctx context.Context, pod *corev1.Pod) corev1.PodStatus {
	c := pod.Spec.Containers[0]
	cs := corev1.ContainerStatus{Name: c.Name, Image: c.Image}
	prev := findStatus(pod.Status.ContainerStatuses, c.Name)
	if prev != nil {
		cs.RestartCount, cs.LastTerminationState = prev.RestartCount, prev.LastTerminationState
	}
	ip := ""
	finish := func() corev1.PodStatus { return buildPodStatus(pod, cs, ip) }
	waiting := func(reason string, err error) corev1.PodStatus {
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: err.Error()}}
		return finish()
	}

	sandboxID, sandbox, podIP, err := m.ensureSandbox(ctx, pod)
	if err != nil {
		return waiting(ReasonCreatePodSandboxError, err)
	}
	ip = podIP
	instances, err := m.containers(ctx, pod.UID, c.Name)
	if err != nil {
		return waiting(ReasonRunContainerError, err)
	}
	next := uint32(0)
	switch {
	case len(instances) > 0:
		next = instances[0].Metadata.GetAttempt() + 1
	case prev != nil && prev.ContainerID != "":
		next = uint32(prev.RestartCount) + 1 // the earlier sandbox and its containers are gone
	}
	var current *runtimeapi.Container
	for _, inst := range instances {
		if inst.PodSandboxId == sandboxID {
			current = inst
			break
		}
	}
	key := pod.UID + "/" + c.Name
	if current == nil {
		return m.startNew(ctx, pod, c, sandboxID, sandbox, next, key, &cs, finish, waiting)
	}
	st, err := m.runtime.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: current.Id})
	if err != nil {
		return waiting(ReasonRunContainerError, err)
	}
	status := st.GetStatus()
	cs.ContainerID, cs.ImageID = "containerd://"+status.Id, status.ImageRef
	cs.RestartCount = int32(status.Metadata.GetAttempt())
	switch status.State {
	case runtimeapi.ContainerState_CONTAINER_RUNNING:
		cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: nanoTime(status.StartedAt)}}
		cs.Ready = true
		// A container that stays up for a minute has recovered; forget its backoff.
		if m.now().Sub(time.Unix(0, status.StartedAt)) >= time.Minute {
			m.mu.Lock()
			delete(m.restarts, key)
			m.mu.Unlock()
		}
		return finish()
	case runtimeapi.ContainerState_CONTAINER_CREATED:
		if _, err := m.runtime.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: current.Id}); err != nil {
			return waiting(ReasonRunContainerError, err)
		}
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: ReasonContainerCreating}}
		return finish()
	case runtimeapi.ContainerState_CONTAINER_EXITED:
		cs.LastTerminationState = corev1.ContainerState{Terminated: terminated(status)}
		m.mu.Lock()
		b := m.restarts[key]
		m.mu.Unlock()
		if m.now().Before(b.next) {
			delay := b.next.Sub(m.now()).Round(time.Second)
			cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: ReasonCrashLoopBackOff,
				Message: fmt.Sprintf("back-off %s restarting failed container=%s pod=%s", delay, c.Name, pod.Name)}}
			return finish()
		}
		result := m.startNew(ctx, pod, c, sandboxID, sandbox, next, key, &cs, finish, waiting)
		m.removeOlderThan(ctx, pod, c.Name, instances, next-1)
		return result
	default:
		return waiting(ReasonRunContainerError, fmt.Errorf("container %s is in state %s", current.Id, status.State))
	}
}

// startNew pulls the image if needed and starts instance `attempt` of c.
func (m *Manager) startNew(ctx context.Context, pod *corev1.Pod, c corev1.Container, sandboxID string,
	sandbox *runtimeapi.PodSandboxConfig, attempt uint32, key string, cs *corev1.ContainerStatus,
	finish func() corev1.PodStatus, waiting func(string, error) corev1.PodStatus) corev1.PodStatus {
	if inBackoff, err := m.ensureImage(ctx, c.Image, sandbox); err != nil {
		if inBackoff {
			return waiting(ReasonImagePullBackOff, err)
		}
		return waiting(ReasonErrImagePull, err)
	}
	created, err := m.runtime.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID, Config: containerConfig(pod, c, attempt), SandboxConfig: sandbox})
	if err != nil {
		return waiting(ReasonCreateContainerError, err)
	}
	_, err = m.runtime.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: created.ContainerId})
	m.mu.Lock()
	m.restarts[key] = m.postpone(m.restarts[key], err)
	m.mu.Unlock()
	cs.ContainerID, cs.RestartCount = "containerd://"+created.ContainerId, int32(attempt)
	if err != nil {
		return waiting(ReasonRunContainerError, err)
	}
	st, err := m.runtime.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: created.ContainerId})
	if err != nil {
		return waiting(ReasonRunContainerError, err)
	}
	cs.ImageID = st.GetStatus().ImageRef
	if st.GetStatus().State != runtimeapi.ContainerState_CONTAINER_RUNNING {
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: ReasonContainerCreating}}
		return finish()
	}
	cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: nanoTime(st.GetStatus().StartedAt)}}
	cs.Ready = true
	return finish()
}

// removeOlderThan deletes instances before attempt keep and their log files;
// the previous instance stays for `logs --previous`.
func (m *Manager) removeOlderThan(ctx context.Context, pod *corev1.Pod, name string, instances []*runtimeapi.Container, keep uint32) {
	for _, inst := range instances {
		if inst.Metadata.GetAttempt() < keep {
			_, _ = m.runtime.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: inst.Id})
		}
	}
	attempts, _ := m.logAttempts(pod, name)
	for _, a := range attempts {
		if uint32(a) < keep {
			_ = os.Remove(filepath.Join(m.PodLogDir(pod), name, fmt.Sprintf("%d.log", a)))
		}
	}
}

// ensureSandbox returns a ready sandbox for the Pod, replacing a sandbox that
// is no longer ready (for example after the node restarted).
func (m *Manager) ensureSandbox(ctx context.Context, pod *corev1.Pod) (string, *runtimeapi.PodSandboxConfig, string, error) {
	sandboxes, err := m.sandboxes(ctx, pod.UID)
	if err != nil {
		return "", nil, "", err
	}
	var id string
	var cfg *runtimeapi.PodSandboxConfig
	if len(sandboxes) > 0 && sandboxes[0].State == runtimeapi.PodSandboxState_SANDBOX_READY {
		id, cfg = sandboxes[0].Id, m.sandboxConfig(pod, sandboxes[0].Metadata.GetAttempt())
		sandboxes = sandboxes[1:]
	}
	for _, old := range sandboxes {
		if err := m.removeSandbox(ctx, old.Id); err != nil {
			return "", nil, "", err
		}
	}
	if id == "" {
		// Failed attempts back off like container restarts; while waiting,
		// the previous error is reported unchanged so the status stays put.
		m.mu.Lock()
		b := m.sandboxRuns[pod.UID]
		m.mu.Unlock()
		if m.now().Before(b.next) {
			return "", nil, "", b.err
		}
		attempt := uint32(0)
		if len(sandboxes) > 0 {
			attempt = sandboxes[0].Metadata.GetAttempt() + 1
		}
		cfg = m.sandboxConfig(pod, attempt)
		resp, err := m.runtime.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{Config: cfg})
		m.mu.Lock()
		if err != nil {
			m.sandboxRuns[pod.UID] = m.postpone(b, err)
		} else {
			delete(m.sandboxRuns, pod.UID)
		}
		m.mu.Unlock()
		if err != nil {
			return "", nil, "", err
		}
		id = resp.PodSandboxId
	}
	st, err := m.runtime.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: id})
	if err != nil {
		return "", nil, "", err
	}
	return id, cfg, st.GetStatus().GetNetwork().GetIp(), nil
}

func (m *Manager) removeSandbox(ctx context.Context, id string) error {
	if _, err := m.runtime.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: id}); err != nil {
		return err
	}
	_, err := m.runtime.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: id})
	return err
}

// ensureImage pulls a missing image with per-image backoff. The bool reports
// whether a backoff is in effect.
func (m *Manager) ensureImage(ctx context.Context, image string, sandbox *runtimeapi.PodSandboxConfig) (bool, error) {
	present, err := m.imagePresent(ctx, image)
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	b := m.pulls[image]
	if present {
		delete(m.pulls, image)
	}
	m.mu.Unlock()
	if present {
		return false, nil
	}
	if m.now().Before(b.next) {
		return true, fmt.Errorf("back-off pulling image %q: %v", image, b.err)
	}
	pullCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	_, err = m.images.PullImage(pullCtx, &runtimeapi.PullImageRequest{Image: &runtimeapi.ImageSpec{Image: image}, SandboxConfig: sandbox})
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.pulls[image] = m.postpone(b, err)
		return false, fmt.Errorf("pull image %q: %w", image, err)
	}
	delete(m.pulls, image)
	return false, nil
}

func nanoTime(ns int64) metav1.Time {
	if ns == 0 {
		return metav1.Time{}
	}
	return metav1.NewTime(time.Unix(0, ns))
}

func terminated(st *runtimeapi.ContainerStatus) *corev1.ContainerStateTerminated {
	reason := st.Reason
	if reason == "" {
		reason = "Error"
		if st.ExitCode == 0 {
			reason = "Completed"
		}
	}
	return &corev1.ContainerStateTerminated{
		ExitCode: st.ExitCode, Reason: reason, Message: st.Message,
		StartedAt: nanoTime(st.StartedAt), FinishedAt: nanoTime(st.FinishedAt),
		ContainerID: "containerd://" + st.Id,
	}
}

func findStatus(list []corev1.ContainerStatus, name string) *corev1.ContainerStatus {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

// buildPodStatus derives phase and conditions from the container status,
// keeping condition transition times when nothing changed.
func buildPodStatus(pod *corev1.Pod, cs corev1.ContainerStatus, ip string) corev1.PodStatus {
	st := corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{cs}, StartTime: pod.Status.StartTime, PodIP: ip}
	st.Conditions = append(st.Conditions, pod.Status.Conditions...)
	if st.PodIP == "" {
		st.PodIP = pod.Status.PodIP
	}
	st.Phase = corev1.PodPending
	if cs.State.Running != nil || cs.LastTerminationState.Terminated != nil {
		st.Phase = corev1.PodRunning
	}
	ready := corev1.ConditionFalse
	if cs.Ready {
		ready = corev1.ConditionTrue
	}
	now := metav1.Now()
	st.SetCondition(corev1.PodCondition{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: now})
	st.SetCondition(corev1.PodCondition{Type: corev1.ContainersReady, Status: ready, LastTransitionTime: now})
	st.SetCondition(corev1.PodCondition{Type: corev1.PodReady, Status: ready, LastTransitionTime: now})
	return st
}
