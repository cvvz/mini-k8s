package kubelet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"mini-k8s/internal/api"
)

// Docker labels marking ownership. The kubelet only ever touches containers
// carrying its cluster and node labels.
const (
	labelCluster   = "mini.cluster"
	labelNode      = "mini.node"
	labelPodUID    = "mini.pod"
	labelPodName   = "mini.pod.name"
	labelNamespace = "mini.pod.namespace"
	labelContainer = "mini.container"
)

// Runtime manages the containers of Pods bound to one node.
type Runtime struct {
	docker                 *dockerClient
	cluster, node, network string
	now                    func() time.Time

	mu       sync.Mutex
	restarts map[string]backoff // by pod UID + container name
	pulls    map[string]backoff // by image
}

type backoff struct {
	attempts int
	next     time.Time
	err      error
}

// Waiting reasons reported in container statuses.
const (
	ReasonContainerCreating = "ContainerCreating"
	ReasonErrImagePull      = "ErrImagePull"
	ReasonImagePullBackOff  = "ImagePullBackOff"
	ReasonCrashLoopBackOff  = "CrashLoopBackOff"
	ReasonRunContainerError = "RunContainerError"
	ReasonCreateError       = "CreateContainerError"
)

// NewRuntime connects to the Docker socket. network must be a dedicated
// user-defined network.
func NewRuntime(socket, cluster, node, network string) (*Runtime, error) {
	if cluster == "" || node == "" {
		return nil, errors.New("runtime requires nonempty cluster and node ownership labels")
	}
	switch {
	case network == "", network == "host", network == "none", network == "bridge", network == "default", strings.HasPrefix(network, "container:"):
		return nil, fmt.Errorf("runtime requires a dedicated container network, got %q", network)
	}
	return &Runtime{docker: newDockerClient(socket), cluster: cluster, node: node, network: network, now: time.Now,
		restarts: map[string]backoff{}, pulls: map[string]backoff{}}, nil
}

func (r *Runtime) owned(labels map[string]string) bool {
	return labels[labelCluster] == r.cluster && labels[labelNode] == r.node && labels[labelPodUID] != ""
}

func (r *Runtime) postpone(prev backoff, err error) backoff {
	if prev.attempts < 6 {
		prev.attempts++
	}
	delay := time.Second << (prev.attempts - 1)
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	prev.next, prev.err = r.now().Add(delay), err
	return prev
}

func containerName(pod *api.Pod, c api.Container) string {
	return fmt.Sprintf("k8s_%s_%s_%s_%s_0", c.Name, pod.Name, pod.Namespace, pod.UID)
}

// podHostname derives a valid hostname (at most 63 characters, not ending
// in '-' or '.') from the Pod name, as the kubelet does.
func podHostname(name string) string {
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-.")
}

// ensureImage returns nil when image is present, pulling it if needed with
// per-image backoff. The bool reports whether a backoff is in effect.
func (r *Runtime) ensureImage(ctx context.Context, image string) (bool, error) {
	ok, err := r.docker.imageExists(ctx, image)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	if ok {
		delete(r.pulls, image)
		r.mu.Unlock()
		return false, nil
	}
	b := r.pulls[image]
	r.mu.Unlock()
	if r.now().Before(b.next) {
		return true, fmt.Errorf("back-off pulling image %q: %v", image, b.err)
	}
	err = r.docker.pull(ctx, image)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.pulls[image] = r.postpone(b, err)
		return false, err
	}
	delete(r.pulls, image)
	return false, nil
}

// SyncPod drives the Pod's container towards running and returns the
// observed status. Runtime failures are reported in the status, never
// hidden; restarts back off exponentially up to 30s.
func (r *Runtime) SyncPod(ctx context.Context, pod *api.Pod) api.PodStatus {
	c := pod.Spec.Containers[0]
	prev := findStatus(pod.Status.ContainerStatuses, c.Name)
	cs := api.ContainerStatus{Name: c.Name, Image: c.Image}
	if prev != nil {
		cs.RestartCount, cs.LastTerminationState = prev.RestartCount, prev.LastTerminationState
	}
	ip := ""
	finish := func() api.PodStatus { return buildPodStatus(pod, cs, ip) }
	waiting := func(reason string, err error) api.PodStatus {
		cs.State = api.ContainerState{Waiting: &api.ContainerStateWaiting{Reason: reason, Message: err.Error()}}
		return finish()
	}

	containers, err := r.docker.list(ctx, labelCluster+"="+r.cluster, labelNode+"="+r.node, labelPodUID+"="+pod.UID, labelContainer+"="+c.Name)
	if err != nil {
		return waiting(ReasonRunContainerError, err)
	}
	var id string
	for _, ctr := range containers {
		if r.owned(ctr.Labels) {
			id = ctr.ID
		}
	}
	if id == "" {
		if inBackoff, err := r.ensureImage(ctx, c.Image); err != nil {
			if inBackoff {
				return waiting(ReasonImagePullBackOff, err)
			}
			return waiting(ReasonErrImagePull, err)
		}
		config := map[string]any{
			"Image": c.Image, "Hostname": podHostname(pod.Name), "Labels": map[string]string{
				labelCluster: r.cluster, labelNode: r.node, labelPodUID: pod.UID,
				labelPodName: pod.Name, labelNamespace: pod.Namespace, labelContainer: c.Name,
			},
			"HostConfig": map[string]any{"NetworkMode": r.network},
		}
		if len(c.Command) > 0 {
			config["Entrypoint"] = c.Command
		}
		if len(c.Args) > 0 {
			config["Cmd"] = c.Args
		}
		var env []string
		for _, e := range c.Env {
			env = append(env, e.Name+"="+e.Value)
		}
		if env != nil {
			config["Env"] = env
		}
		if id, err = r.docker.create(ctx, containerName(pod, c), config); err != nil {
			return waiting(ReasonCreateError, err)
		}
	}
	insp, err := r.docker.inspect(ctx, id)
	if err != nil {
		return waiting(ReasonRunContainerError, err)
	}
	if !r.owned(insp.Config.Labels) || insp.Config.Labels[labelPodUID] != pod.UID {
		return waiting(ReasonRunContainerError, errors.New("container is not owned by this node and Pod"))
	}
	cs.ContainerID, cs.ImageID = "docker://"+insp.ID, insp.Image
	key := pod.UID + "/" + c.Name
	switch insp.State.Status {
	case "running":
	case "created", "exited":
		if insp.State.Status == "exited" {
			cs.LastTerminationState = api.ContainerState{Terminated: terminated(insp)}
		}
		r.mu.Lock()
		b := r.restarts[key]
		r.mu.Unlock()
		if r.now().Before(b.next) {
			delay := b.next.Sub(r.now()).Round(time.Second)
			cs.State = api.ContainerState{Waiting: &api.ContainerStateWaiting{Reason: ReasonCrashLoopBackOff,
				Message: fmt.Sprintf("back-off %s restarting failed container=%s pod=%s", delay, c.Name, pod.Name)}}
			return finish()
		}
		err := r.docker.start(ctx, id)
		r.mu.Lock()
		r.restarts[key] = r.postpone(b, err)
		r.mu.Unlock()
		if err != nil {
			return waiting(ReasonRunContainerError, err)
		}
		if insp.State.Status == "exited" {
			cs.RestartCount++
		}
		if insp, err = r.docker.inspect(ctx, id); err != nil {
			return waiting(ReasonRunContainerError, err)
		}
	default:
		return waiting(ReasonRunContainerError, fmt.Errorf("container state %q: %s", insp.State.Status, insp.State.Error))
	}
	if !insp.State.Running {
		cs.State = api.ContainerState{Waiting: &api.ContainerStateWaiting{Reason: ReasonContainerCreating}}
		return finish()
	}
	// A container that stays up for a minute has recovered; forget its backoff.
	if !insp.State.StartedAt.IsZero() && r.now().Sub(insp.State.StartedAt) >= time.Minute {
		r.mu.Lock()
		delete(r.restarts, key)
		r.mu.Unlock()
	}
	cs.State = api.ContainerState{Running: &api.ContainerStateRunning{StartedAt: api.NewTime(insp.State.StartedAt)}}
	cs.Ready = true
	if n, ok := insp.NetworkSettings.Networks[r.network]; ok {
		ip = n.IPAddress
	}
	return finish()
}

func terminated(insp dockerInspection) *api.ContainerStateTerminated {
	reason := "Error"
	switch {
	case insp.State.OOMKilled:
		reason = "OOMKilled"
	case insp.State.ExitCode == 0:
		reason = "Completed"
	}
	return &api.ContainerStateTerminated{
		ExitCode: int32(insp.State.ExitCode), Reason: reason, Message: insp.State.Error,
		StartedAt: api.NewTime(insp.State.StartedAt), FinishedAt: api.NewTime(insp.State.FinishedAt),
		ContainerID: "docker://" + insp.ID,
	}
}

func findStatus(list []api.ContainerStatus, name string) *api.ContainerStatus {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

// buildPodStatus derives phase and conditions from the container status,
// keeping condition transition times when nothing changed.
func buildPodStatus(pod *api.Pod, cs api.ContainerStatus, ip string) api.PodStatus {
	st := api.PodStatus{ContainerStatuses: []api.ContainerStatus{cs}, StartTime: pod.Status.StartTime, PodIP: ip}
	st.Conditions = append(st.Conditions, pod.Status.Conditions...)
	if st.PodIP == "" && cs.State.Running == nil {
		st.PodIP = pod.Status.PodIP
	}
	st.Phase = api.PodPending
	if cs.State.Running != nil || cs.LastTerminationState.Terminated != nil {
		st.Phase = api.PodRunning
	}
	ready := api.ConditionFalse
	if cs.Ready {
		ready = api.ConditionTrue
	}
	now := api.Now()
	st.SetCondition(api.PodCondition{Type: api.PodInitialized, Status: api.ConditionTrue, LastTransitionTime: now})
	st.SetCondition(api.PodCondition{Type: api.ContainersReady, Status: ready, LastTransitionTime: now})
	st.SetCondition(api.PodCondition{Type: api.PodReady, Status: ready, LastTransitionTime: now})
	return st
}

// KillPod stops the Pod's containers (SIGTERM, then SIGKILL after grace
// seconds) and removes them.
func (r *Runtime) KillPod(ctx context.Context, pod *api.Pod, grace int64) error {
	containers, err := r.docker.list(ctx, labelCluster+"="+r.cluster, labelNode+"="+r.node, labelPodUID+"="+pod.UID)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range containers {
		if !r.owned(c.Labels) {
			continue
		}
		if c.State == "running" {
			if err := r.docker.stop(ctx, c.ID, grace); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := r.docker.remove(ctx, c.ID); err != nil {
			errs = append(errs, err)
		}
	}
	r.forget(pod.UID)
	return errors.Join(errs...)
}

func (r *Runtime) forget(uid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.restarts {
		if strings.HasPrefix(key, uid+"/") {
			delete(r.restarts, key)
		}
	}
}

// Cleanup removes containers of this node whose Pod UID is not in keep.
func (r *Runtime) Cleanup(ctx context.Context, keep map[string]bool) error {
	containers, err := r.docker.list(ctx, labelCluster+"="+r.cluster, labelNode+"="+r.node)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range containers {
		uid := c.Labels[labelPodUID]
		if !r.owned(c.Labels) || keep[uid] {
			continue
		}
		if c.State == "running" {
			_ = r.docker.stop(ctx, c.ID, 2)
		}
		if err := r.docker.remove(ctx, c.ID); err != nil {
			errs = append(errs, fmt.Errorf("remove orphan %s: %w", c.ID, err))
			continue
		}
		r.forget(uid)
	}
	return errors.Join(errs...)
}

// Logs streams a container's logs to w.
func (r *Runtime) Logs(ctx context.Context, pod *api.Pod, container string, follow bool, tail int, w io.Writer) error {
	containers, err := r.docker.list(ctx, labelCluster+"="+r.cluster, labelNode+"="+r.node, labelPodUID+"="+pod.UID, labelContainer+"="+container)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if r.owned(c.Labels) {
			insp, err := r.docker.inspect(ctx, c.ID)
			if err != nil {
				return err
			}
			if insp.Config.TTY {
				return errors.New("logs require a non-TTY container")
			}
			return r.docker.logs(ctx, c.ID, follow, tail, w)
		}
	}
	return fmt.Errorf("container %q of pod %s/%s not found on node %s", container, pod.Namespace, pod.Name, r.node)
}

// Version reports the container runtime version.
func (r *Runtime) Version(ctx context.Context) (string, error) { return r.docker.version(ctx) }
