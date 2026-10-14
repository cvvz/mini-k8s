package scheme

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

func TestObjectJSONShape(t *testing.T) {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}}}
	Pods.SetTypeMeta(p)
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{`"apiVersion":"v1"`, `"kind":"Pod"`, `"metadata":{"name":"a","namespace":"default"}`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	if strings.Contains(s, "status") || strings.Contains(s, "creationTimestamp") {
		t.Errorf("zero status/timestamps must be omitted: %s", s)
	}
	now := metav1.NewTime(time.Date(2026, 10, 14, 1, 2, 3, 999, time.UTC))
	p.CreationTimestamp = now
	data, _ = json.Marshal(p)
	var back corev1.Pod
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !back.CreationTimestamp.Equal(now.Time) || !strings.Contains(string(data), `"2026-10-14T01:02:03Z"`) {
		t.Fatalf("time round trip: %s", data)
	}
}

func TestLookupAndPaths(t *testing.T) {
	for _, name := range []string{"po", "pods", "pod", "Pod"} {
		if r, ok := Lookup(name); !ok || r.Kind != "Pod" {
			t.Fatalf("Lookup(%q) failed", name)
		}
	}
	if r, ok := Lookup("deployments.apps"); !ok || r.Kind != "Deployment" {
		t.Fatal("group-qualified lookup failed")
	}
	if got := Deployments.Path("ns1", "web", "status"); got != "/apis/apps/v1/namespaces/ns1/deployments/web/status" {
		t.Fatal(got)
	}
	if got := Nodes.Path("ignored", "n1"); got != "/api/v1/nodes/n1" {
		t.Fatal(got)
	}
	if got := Pods.Path("", ""); got != "/api/v1/pods" {
		t.Fatal(got)
	}
	if r, ok := ForKind("apps/v1", "Deployment"); !ok || r.Resource != "deployments" {
		t.Fatal("ForKind failed")
	}
	if _, ok := ForKind("v1", "Deployment"); ok {
		t.Fatal("wrong group accepted")
	}
}
