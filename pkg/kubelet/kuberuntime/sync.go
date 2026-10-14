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

// SyncPod drives the Pod towards its desired state and returns the observed
// status. Like the Kubernetes kubelet, it runs the init containers one after
// another, each to successful completion, and then keeps the containers
// running according to the restart policy. An exited container is replaced
// by a new instance (attempt+1) after an exponential backoff of 1s doubling
// to 30s; the previous instance is kept for logs. Runtime failures are
// reported in the status, never hidden. A Pod that reached Succeeded or
// Failed is not touched again except to stop its sandbox.
func (m *Manager) SyncPod(ctx context.Context, pod *corev1.Pod) corev1.PodStatus {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		m.stopSandboxes(ctx, pod.UID)
		return pod.Status
	}
	s := newPodState(pod)
	if restartPolicy(pod) == corev1.RestartPolicyNever && s.ranBefore() && !m.hasReadySandbox(ctx, pod.UID) {
		// The sandbox was lost (the node restarted) after containers ran. Like
		// the kubelet, never start them again: the Pod fails.
		s.loseUnfinished()
		return s.status()
	}
	sandboxID, sandbox, ip, err := m.ensureSandbox(ctx, pod)
	if err != nil {
		s.waitAll(ReasonCreatePodSandboxError, err.Error())
		return s.status()
	}
	s.ip = ip
	r := &podRun{m: m, ctx: ctx, pod: pod, sandboxID: sandboxID, sandbox: sandbox}
	// Volumes are prepared before containers start; a failure only keeps new
	// containers from starting.
	r.volumes, r.volumeErr = m.setupVolumes(pod)
	if r.containersStarted() {
		// Init containers finished before any container was created in this
		// sandbox; never re-run them (a transient runtime error must not send
		// a running Pod back to initializing).
		r.keepInitStatuses(s)
	}
	if s.initialized || r.runInitContainers(s) {
		s.initialized = true
		for i, c := range pod.Spec.Containers {
			cs := &s.containers[i]
			running := r.step(c, cs, restartContainer(restartPolicy(pod))) == outcomeRunning
			// A container is started once its startup probe passed and ready
			// once its readiness probe passed; without probes, when running.
			started := running && (c.StartupProbe == nil || (m.Probes != nil && m.Probes.Started(pod.UID, c.Name, cs.ContainerID)))
			cs.Started = &started
			cs.Ready = started && (c.ReadinessProbe == nil || (m.Probes != nil && m.Probes.Ready(pod.UID, c.Name, cs.ContainerID)))
		}
	}
	return s.status()
}

// podRun carries what one sync of a Pod needs to act on its containers.
type podRun struct {
	m         *Manager
	ctx       context.Context
	pod       *corev1.Pod
	sandboxID string
	sandbox   *runtimeapi.PodSandboxConfig
	volumes   map[string]string // node paths by volume name
	volumeErr error
}

// outcome is a container's situation after one sync step.
type outcome int

const (
	outcomeWaiting   outcome = iota // being created, pulled, backed off or failing to start
	outcomeRunning                  // running
	outcomeSucceeded                // exited 0 and will not be restarted
	outcomeFailed                   // exited non-zero and will not be restarted
)

// restartPolicy returns the Pod's restart policy (Always when unset, for
// Pods stored before the field existed).
func restartPolicy(pod *corev1.Pod) string {
	if pod.Spec.RestartPolicy == "" {
		return corev1.RestartPolicyAlways
	}
	return pod.Spec.RestartPolicy
}

// restartContainer decides whether a container that exited with a code is
// started again under policy.
func restartContainer(policy string) func(int32) bool {
	return func(code int32) bool {
		switch policy {
		case corev1.RestartPolicyNever:
			return false
		case corev1.RestartPolicyOnFailure:
			return code != 0
		}
		return true
	}
}

// runInitContainers advances the init containers and reports whether all of
// them have completed. Init containers that failed are retried unless the
// policy is Never, which fails the Pod.
func (r *podRun) runInitContainers(s *podState) bool {
	policy := restartPolicy(r.pod)
	retry := func(code int32) bool { return code != 0 && policy != corev1.RestartPolicyNever }
	for i, c := range r.pod.Spec.InitContainers {
		switch r.step(c, &s.initContainers[i], retry) {
		case outcomeSucceeded:
			s.initContainers[i].Ready = true
			continue
		case outcomeFailed:
			s.initFailed = true
		}
		for j := i + 1; j < len(s.initContainers); j++ {
			s.initContainers[j].State = waitingState(ReasonPodInitializing, "")
		}
		for j := range s.containers {
			if !s.finished(j) {
				s.containers[j].State = waitingState(ReasonPodInitializing, "")
			}
			s.containers[j].Ready = false
		}
		return false
	}
	return true
}

// step observes one container in the current sandbox and acts on it: starts
// it if it has no instance yet, and restarts it after an exit when restart
// says so. It fills cs and returns the container's outcome.
func (r *podRun) step(c corev1.Container, cs *corev1.ContainerStatus, restart func(int32) bool) outcome {
	m, ctx, pod := r.m, r.ctx, r.pod
	instances, err := m.containers(ctx, pod.UID, c.Name)
	if err != nil {
		cs.State = waitingState(ReasonRunContainerError, err.Error())
		return outcomeWaiting
	}
	next := uint32(0)
	switch {
	case len(instances) > 0:
		next = instances[0].Metadata.GetAttempt() + 1
	case cs.ContainerID != "":
		next = uint32(cs.RestartCount) + 1 // the earlier sandbox and its containers are gone
	}
	var current *runtimeapi.Container
	for _, inst := range instances {
		if inst.PodSandboxId == r.sandboxID {
			current = inst
			break
		}
	}
	key := pod.UID + "/" + c.Name
	if current == nil {
		// A container that finished for good in an earlier sandbox (lost with
		// the node) is not run again.
		if t := cs.State.Terminated; t != nil && !restart(t.ExitCode) {
			if t.ExitCode == 0 {
				return outcomeSucceeded
			}
			return outcomeFailed
		}
		return r.start(c, cs, next, key)
	}
	st, err := m.runtime.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: current.Id})
	if err != nil {
		cs.State = waitingState(ReasonRunContainerError, err.Error())
		return outcomeWaiting
	}
	status := st.GetStatus()
	cs.ContainerID, cs.ImageID = "containerd://"+status.Id, status.ImageRef
	cs.RestartCount = int32(status.Metadata.GetAttempt())
	switch status.State {
	case runtimeapi.ContainerState_CONTAINER_RUNNING:
		cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: nanoTime(status.StartedAt)}}
		// A container that stays up for a minute has recovered; forget its backoff.
		if m.now().Sub(time.Unix(0, status.StartedAt)) >= time.Minute {
			m.mu.Lock()
			delete(m.restarts, key)
			m.mu.Unlock()
		}
		return outcomeRunning
	case runtimeapi.ContainerState_CONTAINER_CREATED:
		if _, err := m.runtime.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: current.Id}); err != nil {
			cs.State = waitingState(ReasonRunContainerError, err.Error())
		} else {
			cs.State = waitingState(ReasonContainerCreating, "")
		}
		return outcomeWaiting
	case runtimeapi.ContainerState_CONTAINER_EXITED:
		term := terminated(status)
		probeKilled := m.killedByProbe(status.Id) && restartPolicy(pod) != corev1.RestartPolicyNever
		if !restart(status.ExitCode) && !probeKilled {
			cs.State = corev1.ContainerState{Terminated: term}
			if status.ExitCode == 0 {
				return outcomeSucceeded
			}
			return outcomeFailed
		}
		cs.LastTerminationState = corev1.ContainerState{Terminated: term}
		m.mu.Lock()
		b := m.restarts[key]
		m.mu.Unlock()
		if m.now().Before(b.next) {
			// The message names the whole backoff period, as in Kubernetes, so
			// it stays the same (and the status is not rewritten) while waiting.
			cs.State = waitingState(ReasonCrashLoopBackOff,
				fmt.Sprintf("back-off %s restarting failed container=%s pod=%s", b.delay, c.Name, pod.Name))
			return outcomeWaiting
		}
		result := r.start(c, cs, next, key)
		m.removeOlderThan(ctx, pod, c.Name, instances, next-1)
		return result
	}
	cs.State = waitingState(ReasonRunContainerError, fmt.Sprintf("container %s is in state %s", current.Id, status.State))
	return outcomeWaiting
}

// containersStarted reports whether any regular container has an instance in
// the current sandbox.
func (r *podRun) containersStarted() bool {
	resp, err := r.m.runtime.ListContainers(r.ctx, &runtimeapi.ListContainersRequest{
		Filter: &runtimeapi.ContainerFilter{PodSandboxId: r.sandboxID}})
	if err != nil {
		return false
	}
	for _, inst := range resp.Containers {
		for _, c := range r.pod.Spec.Containers {
			if inst.Labels[LabelContainerName] == c.Name {
				return true
			}
		}
	}
	return false
}

// keepInitStatuses marks the Pod initialized and reports its init
// containers as completed: from the runtime when their instances can be
// read, else from the last reported status, else as plain completions.
func (r *podRun) keepInitStatuses(s *podState) {
	s.initialized = true
	for i, c := range r.pod.Spec.InitContainers {
		cs := &s.initContainers[i]
		if st := r.currentStatus(c.Name); st != nil && st.State == runtimeapi.ContainerState_CONTAINER_EXITED {
			cs.ContainerID, cs.ImageID = "containerd://"+st.Id, st.ImageRef
			cs.RestartCount = int32(st.Metadata.GetAttempt())
			cs.State, cs.Ready = corev1.ContainerState{Terminated: terminated(st)}, st.ExitCode == 0
			continue
		}
		if p := findStatus(r.pod.Status.InitContainerStatuses, c.Name); p != nil && p.State.Terminated != nil && p.State.Terminated.ExitCode == 0 {
			*cs = *p
			continue
		}
		cs.State, cs.Ready = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}, true
	}
}

// currentStatus returns the runtime status of the named container's instance
// in the current sandbox, or nil.
func (r *podRun) currentStatus(name string) *runtimeapi.ContainerStatus {
	instances, err := r.m.containers(r.ctx, r.pod.UID, name)
	if err != nil {
		return nil
	}
	for _, inst := range instances {
		if inst.PodSandboxId == r.sandboxID {
			st, err := r.m.runtime.ContainerStatus(r.ctx, &runtimeapi.ContainerStatusRequest{ContainerId: inst.Id})
			if err != nil {
				return nil
			}
			return st.GetStatus()
		}
	}
	return nil
}

// start pulls the image if needed and starts instance `attempt` of c.
func (r *podRun) start(c corev1.Container, cs *corev1.ContainerStatus, attempt uint32, key string) outcome {
	m, ctx := r.m, r.ctx
	if r.volumeErr != nil {
		cs.State = waitingState(ReasonCreateContainerConfigError, r.volumeErr.Error())
		return outcomeWaiting
	}
	if c.ImagePullPolicy == corev1.PullNever {
		// Never pull: wait (without backoff) until the image is loaded.
		if present, err := m.imagePresent(ctx, c.Image); err != nil || !present {
			cs.State = waitingState(ReasonErrImageNeverPull,
				fmt.Sprintf("Container image %q is not present with pull policy of Never", c.Image))
			return outcomeWaiting
		}
	} else if inBackoff, err := m.ensureImage(ctx, c.Image, c.ImagePullPolicy == corev1.PullAlways, r.sandbox); err != nil {
		reason := ReasonErrImagePull
		if inBackoff {
			reason = ReasonImagePullBackOff
		}
		cs.State = waitingState(reason, err.Error())
		return outcomeWaiting
	}
	created, err := m.runtime.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: r.sandboxID, Config: containerConfig(r.pod, c, attempt, r.volumes), SandboxConfig: r.sandbox})
	if err != nil {
		cs.State = waitingState(ReasonCreateContainerError, err.Error())
		return outcomeWaiting
	}
	_, err = m.runtime.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: created.ContainerId})
	m.mu.Lock()
	m.restarts[key] = m.postpone(m.restarts[key], err)
	m.mu.Unlock()
	cs.ContainerID, cs.RestartCount = "containerd://"+created.ContainerId, int32(attempt)
	if err != nil {
		cs.State = waitingState(ReasonRunContainerError, err.Error())
		return outcomeWaiting
	}
	st, err := m.runtime.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: created.ContainerId})
	if err != nil {
		cs.State = waitingState(ReasonRunContainerError, err.Error())
		return outcomeWaiting
	}
	cs.ImageID = st.GetStatus().ImageRef
	if st.GetStatus().State != runtimeapi.ContainerState_CONTAINER_RUNNING {
		// Exited already or still starting: the next sync observes it.
		cs.State = waitingState(ReasonContainerCreating, "")
		return outcomeWaiting
	}
	cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: nanoTime(st.GetStatus().StartedAt)}}
	return outcomeRunning
}

func waitingState(reason, message string) corev1.ContainerState {
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}}
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

// hasReadySandbox reports whether the Pod has a ready sandbox.
func (m *Manager) hasReadySandbox(ctx context.Context, uid string) bool {
	sandboxes, err := m.sandboxes(ctx, uid)
	return err != nil || (len(sandboxes) > 0 && sandboxes[0].State == runtimeapi.PodSandboxState_SANDBOX_READY)
}

// stopSandboxes stops the ready sandboxes of a finished Pod, releasing its
// network; its containers and logs stay until the Pod is deleted.
func (m *Manager) stopSandboxes(ctx context.Context, uid string) {
	sandboxes, err := m.sandboxes(ctx, uid)
	if err != nil {
		return
	}
	for _, sb := range sandboxes {
		if sb.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			_, _ = m.runtime.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: sb.Id})
		}
	}
}

// ensureImage pulls the image when it is missing, or always, with per-image
// backoff. The bool reports whether a backoff is in effect.
func (m *Manager) ensureImage(ctx context.Context, image string, always bool, sandbox *runtimeapi.PodSandboxConfig) (bool, error) {
	present, err := m.imagePresent(ctx, image)
	if err != nil {
		return false, err
	}
	present = present && !always
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
