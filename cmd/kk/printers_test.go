package main

import (
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

func TestPodSummaryFollowsKubectl(t *testing.T) {
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	waiting := func(reason string) corev1.ContainerState {
		return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}
	}
	exited := func(code int32, reason string) corev1.ContainerState {
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason}}
	}
	cond := func(t, status string) corev1.PodCondition { return corev1.PodCondition{Type: t, Status: status} }
	spec := corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "i1"}, {Name: "i2"}},
		Containers:     []corev1.Container{{Name: "a"}, {Name: "b"}},
	}
	for name, tc := range map[string]struct {
		status                  corev1.PodStatus
		ready, reason, restarts string
	}{
		"first init running": {corev1.PodStatus{Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{State: running}, {State: waiting("PodInitializing")}},
			ContainerStatuses:     []corev1.ContainerStatus{{State: waiting("PodInitializing")}, {State: waiting("PodInitializing")}}},
			"0/2", "Init:0/2", "0"},
		"second init running": {corev1.PodStatus{Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{State: exited(0, "Completed")}, {State: running}}},
			"0/2", "Init:1/2", "0"},
		"init failed": {corev1.PodStatus{Phase: corev1.PodFailed,
			InitContainerStatuses: []corev1.ContainerStatus{{State: exited(3, "Error")}, {State: waiting("PodInitializing")}}},
			"0/2", "Init:Error", "0"},
		"init crash looping": {corev1.PodStatus{Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{State: waiting("CrashLoopBackOff"), RestartCount: 3}}},
			"0/2", "Init:CrashLoopBackOff", "3"},
		"all running": {corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{cond(corev1.PodInitialized, "True"), cond(corev1.PodReady, "True")},
			InitContainerStatuses: []corev1.ContainerStatus{{State: exited(0, "Completed"), RestartCount: 2}, {State: exited(0, "Completed")}},
			ContainerStatuses:     []corev1.ContainerStatus{{State: running, Ready: true, RestartCount: 1}, {State: running, Ready: true}}},
			"2/2", "Running", "1"},
		"one completed, one running": {corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{cond(corev1.PodReady, "False")},
			InitContainerStatuses: []corev1.ContainerStatus{{State: exited(0, "Completed")}, {State: exited(0, "Completed")}},
			ContainerStatuses:     []corev1.ContainerStatus{{State: exited(0, "Completed")}, {State: running, Ready: true}}},
			"1/2", "NotReady", "0"},
		"one failed": {corev1.PodStatus{Phase: corev1.PodFailed,
			InitContainerStatuses: []corev1.ContainerStatus{{State: exited(0, "Completed")}, {State: exited(0, "Completed")}},
			ContainerStatuses:     []corev1.ContainerStatus{{State: exited(0, "Completed")}, {State: exited(2, "Error")}}},
			"0/2", "Error", "0"},
		"all completed": {corev1.PodStatus{Phase: corev1.PodSucceeded,
			InitContainerStatuses: []corev1.ContainerStatus{{State: exited(0, "Completed")}, {State: exited(0, "Completed")}},
			ContainerStatuses:     []corev1.ContainerStatus{{State: exited(0, "Completed")}, {State: exited(0, "Completed")}}},
			"0/2", "Completed", "0"},
	} {
		p := &corev1.Pod{Spec: spec, Status: tc.status}
		ready, reason, restarts := podReadyCount(p), PodStatusReason(p), podRestarts(p)
		if ready != tc.ready || reason != tc.reason || restarts != tc.restarts {
			t.Errorf("%s: got %s %s %s, want %s %s %s", name, ready, reason, restarts, tc.ready, tc.reason, tc.restarts)
		}
	}
}
