package validation

import (
	"strings"
	"testing"

	"mini-k8s/pkg/api/resource"
	autoscalingv2 "mini-k8s/pkg/apis/autoscaling/v2"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/deepcopy"
	"mini-k8s/pkg/util/ptr"
)

func valid() *autoscalingv2.HorizontalPodAutoscaler {
	h := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
		ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "web"}, MaxReplicas: 3,
		Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{}}}
	autoscalingv2.SetDefaultsHorizontalPodAutoscaler(h)
	return h
}

func TestDefaults(t *testing.T) {
	h := valid()
	m := h.Spec.Metrics[0]
	if *h.Spec.MinReplicas != 1 || m.Type != "Resource" || m.Resource.Name != "cpu" || *m.Resource.Target.AverageUtilization != 80 {
		t.Fatalf("defaults: %+v", h.Spec)
	}
	up, down := h.Spec.Behavior.ScaleUp, h.Spec.Behavior.ScaleDown
	if *up.StabilizationWindowSeconds != 0 || len(up.Policies) != 2 || *down.StabilizationWindowSeconds != 300 ||
		down.Policies[0].Value != 100 || *down.SelectPolicy != "Max" {
		t.Fatalf("behavior: %+v %+v", up, down)
	}
}

func TestValidate(t *testing.T) {
	if errs := ValidateHorizontalPodAutoscaler(valid()); len(errs) != 0 {
		t.Fatalf("valid: %v", errs)
	}
	for _, tc := range []struct {
		change func(*autoscalingv2.HorizontalPodAutoscaler)
		want   string
	}{
		{func(h *autoscalingv2.HorizontalPodAutoscaler) { h.Spec.MinReplicas = ptr.To[int32](5) },
			"spec.maxReplicas: Invalid value: 3: must be greater than or equal to `minReplicas`"},
		{func(h *autoscalingv2.HorizontalPodAutoscaler) { h.Spec.MaxReplicas = 0 }, "spec.maxReplicas: Invalid value: 0: must be greater than 0"},
		{func(h *autoscalingv2.HorizontalPodAutoscaler) { h.Spec.MinReplicas = ptr.To[int32](0) },
			"spec.minReplicas: Invalid value: 0: must be greater than or equal to 1"},
		{func(h *autoscalingv2.HorizontalPodAutoscaler) { h.Spec.ScaleTargetRef.APIVersion = "" },
			`spec.scaleTargetRef.apiVersion: Invalid value: "": apiVersion must specify API group`},
		{func(h *autoscalingv2.HorizontalPodAutoscaler) { h.Spec.ScaleTargetRef.Name = "" }, "spec.scaleTargetRef.name: Required value"},
		{func(h *autoscalingv2.HorizontalPodAutoscaler) {
			h.Spec.Metrics[0].Resource.Target.AverageUtilization = ptr.To[int32](0)
		},
			"spec.metrics[0].resource.target.averageUtilization: Invalid value: 0: must be greater than 0"},
		{func(h *autoscalingv2.HorizontalPodAutoscaler) {
			h.Spec.Metrics[0].Resource.Target.AverageUtilization = nil
		},
			"spec.metrics[0].resource.target.averageUtilization: Required value: must set either a target raw value or a target utilization"},
		{func(h *autoscalingv2.HorizontalPodAutoscaler) {
			h.Spec.Metrics[0].Resource.Target.AverageValue = ptr.To(resource.MustParse("100m"))
		}, "spec.metrics[0].resource.target.averageValue: Forbidden: may not set both a target raw value and a target utilization"},
		{func(h *autoscalingv2.HorizontalPodAutoscaler) { h.Spec.Metrics[0].Type = "External" },
			`spec.metrics[0].type: Unsupported value: "External": supported values: "ContainerResource", "Resource"`},
		{func(h *autoscalingv2.HorizontalPodAutoscaler) { h.Spec.Metrics[0].Resource.Name = "gpu" },
			`spec.metrics[0].resource.name: Unsupported value: "gpu": supported values: "cpu", "memory"`},
		{func(h *autoscalingv2.HorizontalPodAutoscaler) {
			h.Spec.Metrics[0] = autoscalingv2.MetricSpec{Type: "ContainerResource", ContainerResource: &autoscalingv2.ContainerResourceMetricSource{
				Name: "cpu", Target: autoscalingv2.MetricTarget{Type: "Utilization", AverageUtilization: ptr.To[int32](50)}}}
		}, "spec.metrics[0].containerResource.container: Required value: must populate container name"},
		{func(h *autoscalingv2.HorizontalPodAutoscaler) {
			h.Spec.Behavior.ScaleUp.Policies[0].PeriodSeconds = 3600
		},
			"spec.behavior.scaleUp.policies[0].periodSeconds: Invalid value: 3600: must be less than or equal to 1800"},
		{func(h *autoscalingv2.HorizontalPodAutoscaler) {
			h.Spec.Behavior.ScaleDown.SelectPolicy = ptr.To("Sometimes")
		},
			`spec.behavior.scaleDown.selectPolicy: Unsupported value: "Sometimes": supported values: "Disabled", "Max", "Min"`},
		{func(h *autoscalingv2.HorizontalPodAutoscaler) {
			h.Spec.Behavior.ScaleDown.StabilizationWindowSeconds = ptr.To[int32](4000)
		},
			"spec.behavior.scaleDown.stabilizationWindowSeconds: Invalid value: 4000: must be between 0 and 3600"},
	} {
		h := deepcopy.Copy(valid())
		tc.change(h)
		errs := ValidateHorizontalPodAutoscaler(h)
		found := false
		for _, e := range errs {
			found = found || strings.Contains(e, tc.want)
		}
		if !found {
			t.Errorf("errors %q lack %q", errs, tc.want)
		}
	}
}
