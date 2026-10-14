package kuberuntime

import (
	"fmt"
	"strings"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

// ReasonPodInitializing is the waiting reason of containers that wait for
// init containers.
const ReasonPodInitializing = "PodInitializing"

// podState accumulates the observed state of a Pod during one sync.
type podState struct {
	pod            *corev1.Pod
	ip             string
	initContainers []corev1.ContainerStatus
	containers     []corev1.ContainerStatus
	// initialized reports that every init container completed.
	initialized bool
	// initFailed reports an init container that failed for good
	// (restartPolicy Never), which fails the Pod.
	initFailed bool
	// lost reports that the sandbox of a restartPolicy Never Pod was lost.
	lost bool
}

// newPodState starts from the previously reported statuses so restart counts,
// last states and container IDs survive the loss of a sandbox. A container's
// final termination is carried too, so it is not run again in a new sandbox
// (init containers do run again, as in Kubernetes).
func newPodState(pod *corev1.Pod) *podState {
	carry := func(specs []corev1.Container, prev []corev1.ContainerStatus, keepTerminated bool) []corev1.ContainerStatus {
		out := make([]corev1.ContainerStatus, len(specs))
		for i, c := range specs {
			out[i] = corev1.ContainerStatus{Name: c.Name, Image: c.Image}
			if p := findStatus(prev, c.Name); p != nil {
				out[i].RestartCount, out[i].LastTerminationState, out[i].ContainerID = p.RestartCount, p.LastTerminationState, p.ContainerID
				if keepTerminated && p.State.Terminated != nil {
					out[i].State.Terminated = p.State.Terminated
				}
			}
		}
		return out
	}
	return &podState{pod: pod, initialized: len(pod.Spec.InitContainers) == 0,
		initContainers: carry(pod.Spec.InitContainers, pod.Status.InitContainerStatuses, false),
		containers:     carry(pod.Spec.Containers, pod.Status.ContainerStatuses, true)}
}

// finished reports whether container i terminated and will not be restarted.
func (s *podState) finished(i int) bool {
	t := s.containers[i].State.Terminated
	return t != nil && !restartContainer(restartPolicy(s.pod))(t.ExitCode)
}

// ranBefore reports whether any container of the Pod was ever created.
func (s *podState) ranBefore() bool {
	for _, list := range [][]corev1.ContainerStatus{s.initContainers, s.containers} {
		for _, cs := range list {
			if cs.ContainerID != "" {
				return true
			}
		}
	}
	return false
}

// loseUnfinished records that the Pod's sandbox and containers were lost:
// containers that had not finished are reported as terminated with an
// unknown status (exit code 137, like the kubelet), which fails the Pod.
func (s *podState) loseUnfinished() {
	lost := &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "ContainerStatusUnknown",
		Message: "The container could not be located when the pod was terminated"}
	for _, list := range [][]corev1.ContainerStatus{s.initContainers, s.containers} {
		for i := range list {
			if p := findStatus(append(append([]corev1.ContainerStatus{}, s.pod.Status.InitContainerStatuses...), s.pod.Status.ContainerStatuses...), list[i].Name); p != nil && p.State.Terminated != nil {
				list[i].State, list[i].Ready = p.State, p.Ready
				continue
			}
			list[i].State, list[i].Ready = corev1.ContainerState{Terminated: lost}, false
		}
	}
	s.initialized = true
	for _, cs := range s.initContainers {
		if t := cs.State.Terminated; t == nil || t.ExitCode != 0 {
			s.initialized = false
		}
	}
	s.lost = true
}

// waitAll marks every container as waiting for reason (the sandbox failed).
func (s *podState) waitAll(reason, message string) {
	for _, list := range [][]corev1.ContainerStatus{s.initContainers, s.containers} {
		for i := range list {
			list[i].State, list[i].Ready = waitingState(reason, message), false
		}
	}
}

// status assembles the Pod status: phase as computed by the Kubernetes
// kubelet (getPhase) and the Initialized, ContainersReady and Ready
// conditions, keeping transition times of unchanged conditions.
func (s *podState) status() corev1.PodStatus {
	pod := s.pod
	st := corev1.PodStatus{StartTime: pod.Status.StartTime, PodIP: s.ip, QOSClass: pod.Status.QOSClass,
		InitContainerStatuses: s.initContainers, ContainerStatuses: s.containers}
	if len(st.InitContainerStatuses) == 0 {
		st.InitContainerStatuses = nil
	}
	st.Conditions = append(st.Conditions, pod.Status.Conditions...)
	if st.PodIP == "" {
		st.PodIP = pod.Status.PodIP
	}
	st.Phase = s.phase()
	now := metav1.Now()

	initialized := corev1.PodCondition{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: now}
	if !s.initialized {
		var pending []string
		for _, cs := range s.initContainers {
			if !cs.Ready {
				pending = append(pending, cs.Name)
			}
		}
		initialized.Status, initialized.Reason = corev1.ConditionFalse, "ContainersNotInitialized"
		initialized.Message = fmt.Sprintf("containers with incomplete status: [%s]", strings.Join(pending, " "))
	}
	st.SetCondition(initialized)

	ready := corev1.PodCondition{Status: corev1.ConditionTrue, LastTransitionTime: now}
	var unready []string
	for _, cs := range s.containers {
		if !cs.Ready {
			unready = append(unready, cs.Name)
		}
	}
	switch {
	case st.Phase == corev1.PodFailed:
		ready.Status, ready.Reason = corev1.ConditionFalse, "PodFailed"
	case st.Phase == corev1.PodSucceeded:
		ready.Status, ready.Reason = corev1.ConditionFalse, "PodCompleted"
	case len(unready) > 0:
		ready.Status, ready.Reason = corev1.ConditionFalse, "ContainersNotReady"
		ready.Message = fmt.Sprintf("containers with unready status: [%s]", strings.Join(unready, " "))
	}
	for _, t := range []string{corev1.ContainersReady, corev1.PodReady} {
		c := ready
		c.Type = t
		st.SetCondition(c)
	}
	return st
}

// phase follows the kubelet's getPhase: Pending until initialized and while
// a container has never run, Running while something runs or will be
// restarted, Succeeded when all containers exited 0 for good, Failed when
// all stopped for good and one failed (or an init container failed under
// restartPolicy Never).
func (s *podState) phase() string {
	if s.lost {
		return corev1.PodFailed
	}
	if !s.initialized {
		if s.initFailed {
			return corev1.PodFailed
		}
		return corev1.PodPending
	}
	var running, waiting, stopped, succeeded int
	for _, cs := range s.containers {
		switch {
		case cs.State.Running != nil:
			running++
		case cs.State.Terminated != nil:
			stopped++
			if cs.State.Terminated.ExitCode == 0 {
				succeeded++
			}
		case cs.LastTerminationState.Terminated != nil:
			stopped++ // waiting to be restarted
		default:
			waiting++
		}
	}
	switch {
	case waiting > 0:
		return corev1.PodPending
	case running > 0:
		return corev1.PodRunning
	case stopped > 0:
		switch {
		case restartPolicy(s.pod) == corev1.RestartPolicyAlways:
			return corev1.PodRunning
		case stopped == succeeded:
			return corev1.PodSucceeded
		case restartPolicy(s.pod) == corev1.RestartPolicyNever:
			return corev1.PodFailed
		}
		return corev1.PodRunning
	}
	return corev1.PodPending
}

func findStatus(list []corev1.ContainerStatus, name string) *corev1.ContainerStatus {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}
