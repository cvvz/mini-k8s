package main

import (
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
	networkingv1 "mini-k8s/pkg/apis/networking/v1"
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

func TestServiceEndpointsAndIngressColumns(t *testing.T) {
	svc := &corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{
		{Port: 80, NodePort: 30875, Protocol: "TCP"}, {Port: 81, Protocol: "UDP"}}}}
	if got := servicePorts(svc); got != "80:30875/TCP,81/UDP" {
		t.Errorf("PORT(S) = %q", got)
	}
	addrs := func(ips ...string) []corev1.EndpointAddress {
		var out []corev1.EndpointAddress
		for _, ip := range ips {
			out = append(out, corev1.EndpointAddress{IP: ip})
		}
		return out
	}
	ep := &corev1.Endpoints{Subsets: []corev1.EndpointSubset{{
		Addresses:         addrs("10.244.1.176", "10.244.1.212"),
		NotReadyAddresses: addrs("10.244.2.9"),
		Ports:             []corev1.EndpointPort{{Name: "http", Port: 8080}, {Name: "alt", Port: 9090}},
	}}}
	if got := formatEndpoints(ep); got != "10.244.1.176:8080,10.244.1.212:8080,10.244.1.176:9090 + 1 more..." {
		t.Errorf("ENDPOINTS = %q", got)
	}
	if got := formatEndpoints(&corev1.Endpoints{}); got != "<none>" {
		t.Errorf("empty ENDPOINTS = %q", got)
	}
	ing := &networkingv1.Ingress{Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "foo.example.com"}, {}}}}
	if got := ingressHosts(ing); got != "foo.example.com" {
		t.Errorf("HOSTS = %q", got)
	}
	if got := ingressHosts(&networkingv1.Ingress{Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{}}}}); got != "*" {
		t.Errorf("HOSTS without hosts = %q", got)
	}
}
