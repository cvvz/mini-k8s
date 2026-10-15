package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"mini-k8s/pkg/api/resource"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	metricsv1beta1 "mini-k8s/pkg/apis/metrics/v1beta1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/integration/framework"
)

func TestTop(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := context.Background()
	if _, err := kk(t, c, "", "top", "nodes"); err == nil || err.Error() != "Metrics API not available" {
		t.Fatalf("without metrics-server: %v", err)
	}
	usage := func(cpu, mem string) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)}
	}
	pods := []metricsv1beta1.PodMetrics{
		{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}, Containers: []metricsv1beta1.ContainerMetrics{
			{Name: "app", Usage: usage("117586291n", "131288Ki")}, {Name: "proxy", Usage: usage("2500000n", "8Mi")}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "batch", Namespace: "default"}, Containers: []metricsv1beta1.ContainerMetrics{
			{Name: "work", Usage: usage("900m", "64Mi")}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "dns", Namespace: "kube-system"}, Containers: []metricsv1beta1.ContainerMetrics{
			{Name: "dns", Usage: usage("1m", "5Mi")}}},
	}
	ms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/apis/metrics.k8s.io/v1beta1")
		var out any
		switch {
		case path == "/nodes":
			out = metricsv1beta1.NodeMetricsList{Items: []metricsv1beta1.NodeMetrics{
				{ObjectMeta: metav1.ObjectMeta{Name: "worker-2"}, Usage: usage("422000000n", "1893Mi")},
				{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}, Usage: usage("330m", "2907Mi")}}}
		case path == "/pods":
			out = metricsv1beta1.PodMetricsList{Items: pods}
		case path == "/namespaces/default/pods":
			out = metricsv1beta1.PodMetricsList{Items: pods[:2]}
		case path == "/namespaces/default/pods/web":
			out = pods[0]
		default:
			w.WriteHeader(http.StatusNotFound)
			out = metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Reason: metav1.ReasonNotFound, Message: "not found", Code: 404}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(ms.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(ms.URL, "http://"))
	p, _ := strconv.Atoi(port)
	if _, err := client.Endpoints(c, "kube-system").Create(ctx, &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "metrics-server"},
		Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: host}}, Ports: []corev1.EndpointPort{{Port: int32(p)}}}}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"worker-1", "worker-2"} {
		n, err := client.Nodes(c).Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
		if err != nil {
			t.Fatal(err)
		}
		n.Status.Allocatable = usage("4", "13Gi")
		if _, err := client.Nodes(c).UpdateStatus(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	lines := func(out string) []string {
		var rows []string
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			rows = append(rows, strings.Join(strings.Fields(l), " "))
		}
		return rows
	}
	expect := func(argv []string, want ...string) {
		t.Helper()
		out, err := kk(t, c, "", argv...)
		if err != nil || strings.Join(lines(out), "\n") != strings.Join(want, "\n") {
			t.Fatalf("kk %v: %v\n%s", argv, err, out)
		}
	}
	expect([]string{"top", "nodes"}, "NAME CPU(cores) CPU(%) MEMORY(bytes) MEMORY(%)",
		"worker-1 330m 8% 2907Mi 21%", "worker-2 422m 10% 1893Mi 14%")
	expect([]string{"top", "nodes", "--sort-by", "memory"}, "NAME CPU(cores) CPU(%) MEMORY(bytes) MEMORY(%)",
		"worker-1 330m 8% 2907Mi 21%", "worker-2 422m 10% 1893Mi 14%")
	expect([]string{"top", "pods"}, "NAME CPU(cores) MEMORY(bytes)", "batch 900m 64Mi", "web 121m 136Mi")
	expect([]string{"top", "pods", "--sort-by", "cpu"}, "NAME CPU(cores) MEMORY(bytes)", "batch 900m 64Mi", "web 121m 136Mi")
	expect([]string{"top", "pod", "web", "--containers"}, "POD NAME CPU(cores) MEMORY(bytes)", "web app 118m 128Mi", "web proxy 3m 8Mi")
	expect([]string{"top", "pods", "-A"}, "NAMESPACE NAME CPU(cores) MEMORY(bytes)", "default batch 900m 64Mi", "default web 121m 136Mi",
		"kube-system dns 1m 5Mi")
	if _, err := kk(t, c, "", "top", "pod", "ghost"); !client.IsNotFound(err) {
		t.Fatalf("a missing Pod: %v", err)
	}
	if _, err := kk(t, c, "", "top", "deployments"); err == nil || !strings.HasPrefix(err.Error(), "usage: kk top") {
		t.Fatalf("bad type: %v", err)
	}
}
