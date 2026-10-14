package validation

import (
	"strings"
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

func podSpec() *corev1.PodSpec {
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "setup", Image: "img"}},
		Containers:     []corev1.Container{{Name: "web", Image: "img"}, {Name: "sidecar", Image: "img"}},
	}
	corev1.SetDefaultsPodSpec(spec)
	return spec
}

func TestValidatePodSpecAcceptsInitAndMultipleContainers(t *testing.T) {
	spec := podSpec()
	if spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Fatalf("restartPolicy defaults to Always, got %q", spec.RestartPolicy)
	}
	if errs := ValidatePodSpec("spec", spec); len(errs) != 0 {
		t.Fatalf("valid spec rejected: %v", errs)
	}
	for _, policy := range []string{corev1.RestartPolicyOnFailure, corev1.RestartPolicyNever} {
		spec.RestartPolicy = policy
		if errs := ValidatePodSpec("spec", spec); len(errs) != 0 {
			t.Fatalf("%s rejected: %v", policy, errs)
		}
	}
}

func TestValidatePodSpecRejectsInvalidSpecs(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*corev1.PodSpec)
		want   string
	}{
		"no containers":         {func(s *corev1.PodSpec) { s.Containers = nil }, "spec.containers: Required value"},
		"duplicate name":        {func(s *corev1.PodSpec) { s.Containers[1].Name = "web" }, `spec.containers[1].name: Duplicate value: "web"`},
		"name shared with init": {func(s *corev1.PodSpec) { s.InitContainers[0].Name = "web" }, `spec.containers[0].name: Duplicate value: "web"`},
		"init without image":    {func(s *corev1.PodSpec) { s.InitContainers[0].Image = "" }, "spec.initContainers[0].image: Required value"},
		"bad restart policy":    {func(s *corev1.PodSpec) { s.RestartPolicy = "Sometimes" }, `spec.restartPolicy: Unsupported value: "Sometimes"`},
	} {
		spec := podSpec()
		tc.mutate(spec)
		errs := strings.Join(ValidatePodSpec("spec", spec), "\n")
		if !strings.Contains(errs, tc.want) {
			t.Errorf("%s: want %q in %q", name, tc.want, errs)
		}
	}
}
