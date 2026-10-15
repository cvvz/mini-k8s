// Package validation checks autoscaling objects like Kubernetes.
package validation

import (
	"fmt"

	autoscalingv2 "mini-k8s/pkg/apis/autoscaling/v2"
)

// ValidateHorizontalPodAutoscaler validates a complete autoscaler. Only
// resource metrics (CPU and memory, of whole Pods or one container) are
// supported, as mini has no custom or external metrics API.
func ValidateHorizontalPodAutoscaler(h *autoscalingv2.HorizontalPodAutoscaler) []string {
	var errs []string
	s := h.Spec
	ref := s.ScaleTargetRef
	if ref.Kind == "" {
		errs = append(errs, "spec.scaleTargetRef.kind: Required value")
	}
	if ref.Name == "" {
		errs = append(errs, "spec.scaleTargetRef.name: Required value")
	}
	if ref.APIVersion == "" {
		errs = append(errs, `spec.scaleTargetRef.apiVersion: Invalid value: "": apiVersion must specify API group`)
	}
	minReplicas := int32(1)
	if s.MinReplicas != nil {
		minReplicas = *s.MinReplicas
		if minReplicas < 1 {
			errs = append(errs, fmt.Sprintf("spec.minReplicas: Invalid value: %d: must be greater than or equal to 1", minReplicas))
		}
	}
	if s.MaxReplicas < 1 {
		errs = append(errs, fmt.Sprintf("spec.maxReplicas: Invalid value: %d: must be greater than 0", s.MaxReplicas))
	}
	if s.MaxReplicas < minReplicas {
		errs = append(errs, fmt.Sprintf("spec.maxReplicas: Invalid value: %d: must be greater than or equal to `minReplicas`", s.MaxReplicas))
	}
	for i, m := range s.Metrics {
		path := fmt.Sprintf("spec.metrics[%d]", i)
		switch m.Type {
		case autoscalingv2.ResourceMetricSourceType:
			if m.Resource == nil {
				errs = append(errs, path+".resource: Required value: must populate information for the given metric source")
				continue
			}
			errs = append(errs, validateResource(path+".resource", m.Resource.Name, m.Resource.Target)...)
		case autoscalingv2.ContainerResourceMetricSourceType:
			if m.ContainerResource == nil {
				errs = append(errs, path+".containerResource: Required value: must populate information for the given metric source")
				continue
			}
			if m.ContainerResource.Container == "" {
				errs = append(errs, path+".containerResource.container: Required value: must populate container name")
			}
			errs = append(errs, validateResource(path+".containerResource", m.ContainerResource.Name, m.ContainerResource.Target)...)
		default:
			errs = append(errs, fmt.Sprintf(`%s.type: Unsupported value: %q: supported values: "ContainerResource", "Resource"`, path, m.Type))
		}
	}
	if b := s.Behavior; b != nil {
		errs = append(errs, validateRules("spec.behavior.scaleUp", b.ScaleUp)...)
		errs = append(errs, validateRules("spec.behavior.scaleDown", b.ScaleDown)...)
	}
	return errs
}

func validateResource(path, name string, t autoscalingv2.MetricTarget) []string {
	var errs []string
	if name != "cpu" && name != "memory" {
		errs = append(errs, fmt.Sprintf(`%s.name: Unsupported value: %q: supported values: "cpu", "memory"`, path, name))
	}
	switch t.Type {
	case autoscalingv2.UtilizationMetricType, autoscalingv2.AverageValueMetricType:
	case autoscalingv2.ValueMetricType:
		errs = append(errs, fmt.Sprintf("%s.target.type: Invalid value: %q: must be either Utilization or AverageValue", path, t.Type))
	default:
		errs = append(errs, fmt.Sprintf("%s.target.type: Invalid value: %q: must be either Utilization, Value, or AverageValue", path, t.Type))
	}
	if t.AverageUtilization == nil && t.AverageValue == nil {
		errs = append(errs, path+".target.averageUtilization: Required value: must set either a target raw value or a target utilization")
	}
	if t.AverageUtilization != nil && t.AverageValue != nil {
		errs = append(errs, path+".target.averageValue: Forbidden: may not set both a target raw value and a target utilization")
	}
	if u := t.AverageUtilization; u != nil && *u < 1 {
		errs = append(errs, fmt.Sprintf("%s.target.averageUtilization: Invalid value: %d: must be greater than 0", path, *u))
	}
	if v := t.AverageValue; v != nil && v.IsZero() {
		errs = append(errs, fmt.Sprintf("%s.target.averageValue: Invalid value: %q: must be positive", path, v.String()))
	}
	return errs
}

func validateRules(path string, r *autoscalingv2.HPAScalingRules) []string {
	if r == nil {
		return nil
	}
	var errs []string
	if w := r.StabilizationWindowSeconds; w != nil && (*w < 0 || *w > 3600) {
		errs = append(errs, fmt.Sprintf("%s.stabilizationWindowSeconds: Invalid value: %d: must be between 0 and 3600", path, *w))
	}
	if p := r.SelectPolicy; p != nil {
		switch *p {
		case autoscalingv2.MaxChangePolicySelect, autoscalingv2.MinChangePolicySelect, autoscalingv2.DisabledPolicySelect:
		default:
			errs = append(errs, fmt.Sprintf(`%s.selectPolicy: Unsupported value: %q: supported values: "Disabled", "Max", "Min"`, path, *p))
		}
	}
	for i, p := range r.Policies {
		pp := fmt.Sprintf("%s.policies[%d]", path, i)
		if p.Type != autoscalingv2.PodsScalingPolicy && p.Type != autoscalingv2.PercentScalingPolicy {
			errs = append(errs, fmt.Sprintf(`%s.type: Unsupported value: %q: supported values: "Percent", "Pods"`, pp, p.Type))
		}
		if p.Value <= 0 {
			errs = append(errs, fmt.Sprintf("%s.value: Invalid value: %d: must be greater than zero", pp, p.Value))
		}
		switch {
		case p.PeriodSeconds <= 0:
			errs = append(errs, fmt.Sprintf("%s.periodSeconds: Invalid value: %d: must be greater than zero", pp, p.PeriodSeconds))
		case p.PeriodSeconds > 1800:
			errs = append(errs, fmt.Sprintf("%s.periodSeconds: Invalid value: %d: must be less than or equal to 1800", pp, p.PeriodSeconds))
		}
	}
	return errs
}
