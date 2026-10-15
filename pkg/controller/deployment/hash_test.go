package deployment

import (
	"testing"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

func TestEqualIgnoreHash(t *testing.T) {
	stored := &corev1.PodTemplateSpec{ // as an older version stored it: no newer defaults
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web", appsv1.DefaultDeploymentUniqueLabelKey: "older"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img:1"}}},
	}
	current := &corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img:1"}}}}
	corev1.SetDefaultsPodSpec(&current.Spec)
	if !EqualIgnoreHash(stored, current) {
		t.Fatal("templates differing only by defaults and the hash label are equal")
	}
	if TemplateHash(stored) == TemplateHash(current) {
		t.Fatal("the test needs templates whose hashes differ")
	}
	current.Spec.Containers[0].Image = "img:2"
	if EqualIgnoreHash(stored, current) {
		t.Fatal("a different image is a different template")
	}
	if stored.Labels[appsv1.DefaultDeploymentUniqueLabelKey] != "older" || stored.Spec.DNSPolicy != "" {
		t.Fatal("EqualIgnoreHash must not modify its arguments")
	}
}
