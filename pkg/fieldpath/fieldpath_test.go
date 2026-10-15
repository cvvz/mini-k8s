package fieldpath

import (
	"testing"

	"mini-k8s/pkg/api/resource"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/ptr"
)

func TestPodFieldValue(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "u",
		Labels: map[string]string{"b": "2", "a": "1"}, Annotations: map[string]string{"q": `x"y`}},
		Spec: corev1.PodSpec{NodeName: "n1"}}
	for path, want := range map[string]string{
		"metadata.name": "p", "metadata.namespace": "ns", "metadata.uid": "u", "spec.nodeName": "n1",
		"metadata.labels": "a=\"1\"\nb=\"2\"", "metadata.annotations": `q="x\"y"`, "metadata.labels['b']": "2",
		"metadata.labels['missing']": "", "status.podIP": "10.0.0.5", "status.podIPs": "10.0.0.5", "status.hostIP": "172.18.0.2",
	} {
		if got, err := PodFieldValue(pod, path, "10.0.0.5", "172.18.0.2"); err != nil || got != want {
			t.Errorf("%s = %q %v, want %q", path, got, err, want)
		}
	}
	if _, err := PodFieldValue(pod, "spec.nodeName['x']", "", ""); err == nil {
		t.Error("only labels and annotations take subscripts")
	}
	if p, s, ok := SplitMaybeSubscriptedPath("metadata.annotations['a.b/c']"); !ok || p != "metadata.annotations" || s != "a.b/c" {
		t.Errorf("split: %q %q %v", p, s, ok)
	}
}

func TestResourceValue(t *testing.T) {
	c := &corev1.Container{Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}}}
	node := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}
	q := func(s string) *resource.Quantity { return ptr.To(resource.MustParse(s)) }
	for _, tc := range []struct {
		res     string
		divisor *resource.Quantity
		want    string
	}{
		{"limits.cpu", nil, "1"}, {"limits.cpu", q("1m"), "250"}, {"requests.cpu", q("1m"), "100"},
		{"requests.memory", q("1Mi"), "32"}, {"limits.memory", q("1Gi"), "8"}, {"requests.memory", nil, "33554432"},
	} {
		if got, err := ResourceValue(&corev1.ResourceFieldSelector{Resource: tc.res, Divisor: tc.divisor}, c, node); err != nil || got != tc.want {
			t.Errorf("%s/%v = %q %v, want %q", tc.res, tc.divisor, got, err, tc.want)
		}
	}
}
