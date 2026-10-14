package validation

import (
	"strings"
	"testing"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/ptr"
)

func deployment() *appsv1.Deployment {
	labels := map[string]string{"app": "web"}
	return &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
		Replicas: ptr.To[int32](1), Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyAlways, Containers: []corev1.Container{{Name: "web", Image: "img", ImagePullPolicy: corev1.PullIfNotPresent}}}},
	}}
}

func TestValidateDeployment(t *testing.T) {
	if errs := ValidateDeployment(deployment()); len(errs) != 0 {
		t.Fatalf("valid deployment rejected: %v", errs)
	}
	d := deployment()
	d.Spec.Replicas = ptr.To[int32](-1)
	d.Spec.Selector.MatchLabels = map[string]string{"app": "other"}
	d.Spec.Template.Spec.Containers[0].Image = ""
	errs := strings.Join(ValidateDeployment(d), "\n")
	for _, want := range []string{"spec.replicas", "does not match", "spec.template.spec.containers[0].image"} {
		if !strings.Contains(errs, want) {
			t.Errorf("missing %q in:\n%s", want, errs)
		}
	}
	never := deployment()
	never.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
	if errs := strings.Join(ValidateDeployment(never), "\n"); !strings.Contains(errs, `spec.template.spec.restartPolicy: Unsupported value: "Never"`) {
		t.Errorf("Deployment template must require restartPolicy Always: %s", errs)
	}
	changed := deployment()
	changed.Spec.Selector.MatchLabels = map[string]string{"app": "web", "tier": "x"}
	if len(ValidateDeploymentUpdate(changed, deployment())) != 1 {
		t.Error("selector change accepted")
	}
}
