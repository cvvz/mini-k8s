// Package v2 contains the autoscaling/v2 types (autoscalingv2): the
// HorizontalPodAutoscaler, which scales a workload's replicas to keep the
// CPU or memory of its Pods at a target, and Scale, the view of a
// workload's replica count that the scale subresource serves
// (autoscaling/v1 in Kubernetes).
package v2

import (
	"mini-k8s/pkg/api/resource"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/ptr"
)

// HorizontalPodAutoscaler scales a target between MinReplicas and
// MaxReplicas.
type HorizontalPodAutoscaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              HorizontalPodAutoscalerSpec   `json:"spec"`
	Status            HorizontalPodAutoscalerStatus `json:"status,omitzero"`
}

// HorizontalPodAutoscalerSpec is the target, the bounds and the metrics.
type HorizontalPodAutoscalerSpec struct {
	ScaleTargetRef CrossVersionObjectReference `json:"scaleTargetRef"`
	MinReplicas    *int32                      `json:"minReplicas,omitempty"`
	MaxReplicas    int32                       `json:"maxReplicas"`
	// Metrics default to CPU at 80% of the requests.
	Metrics  []MetricSpec                     `json:"metrics,omitempty"`
	Behavior *HorizontalPodAutoscalerBehavior `json:"behavior,omitempty"`
}

// CrossVersionObjectReference names the scaled workload.
type CrossVersionObjectReference struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	APIVersion string `json:"apiVersion,omitempty"`
}

// Metric source types; mini supports the resource metrics only.
const (
	ResourceMetricSourceType          = "Resource"
	ContainerResourceMetricSourceType = "ContainerResource"
	PodsMetricSourceType              = "Pods"
	ObjectMetricSourceType            = "Object"
	ExternalMetricSourceType          = "External"
)

// MetricSpec is one metric to scale on.
type MetricSpec struct {
	Type              string                         `json:"type"`
	Resource          *ResourceMetricSource          `json:"resource,omitempty"`
	ContainerResource *ContainerResourceMetricSource `json:"containerResource,omitempty"`
}

// ResourceMetricSource is the CPU or memory of the whole Pods.
type ResourceMetricSource struct {
	Name   string       `json:"name"`
	Target MetricTarget `json:"target"`
}

// ContainerResourceMetricSource is the CPU or memory of one container of
// each Pod.
type ContainerResourceMetricSource struct {
	Name      string       `json:"name"`
	Container string       `json:"container"`
	Target    MetricTarget `json:"target"`
}

// Metric target types.
const (
	UtilizationMetricType  = "Utilization"
	ValueMetricType        = "Value"
	AverageValueMetricType = "AverageValue"
)

// MetricTarget is the value to keep: a percentage of the requests
// (Utilization) or an average amount per Pod (AverageValue).
type MetricTarget struct {
	Type               string             `json:"type"`
	Value              *resource.Quantity `json:"value,omitempty"`
	AverageValue       *resource.Quantity `json:"averageValue,omitempty"`
	AverageUtilization *int32             `json:"averageUtilization,omitempty"`
}

// HorizontalPodAutoscalerBehavior shapes scaling up and down.
type HorizontalPodAutoscalerBehavior struct {
	ScaleUp   *HPAScalingRules `json:"scaleUp,omitempty"`
	ScaleDown *HPAScalingRules `json:"scaleDown,omitempty"`
}

// Scaling policy selection.
const (
	MaxChangePolicySelect = "Max"
	MinChangePolicySelect = "Min"
	DisabledPolicySelect  = "Disabled"
)

// HPAScalingRules bound one direction: the recommendations of the
// stabilization window are considered, and the policies limit the change
// per period.
type HPAScalingRules struct {
	StabilizationWindowSeconds *int32             `json:"stabilizationWindowSeconds,omitempty"`
	SelectPolicy               *string            `json:"selectPolicy,omitempty"`
	Policies                   []HPAScalingPolicy `json:"policies,omitempty"`
}

// Scaling policy types.
const (
	PodsScalingPolicy    = "Pods"
	PercentScalingPolicy = "Percent"
)

// HPAScalingPolicy allows a change of Value Pods (or percent) per
// PeriodSeconds.
type HPAScalingPolicy struct {
	Type          string `json:"type"`
	Value         int32  `json:"value"`
	PeriodSeconds int32  `json:"periodSeconds"`
}

// HorizontalPodAutoscalerStatus is maintained by the controller.
type HorizontalPodAutoscalerStatus struct {
	ObservedGeneration *int64                             `json:"observedGeneration,omitempty"`
	LastScaleTime      *metav1.Time                       `json:"lastScaleTime,omitempty"`
	CurrentReplicas    int32                              `json:"currentReplicas,omitempty"`
	DesiredReplicas    int32                              `json:"desiredReplicas"`
	CurrentMetrics     []MetricStatus                     `json:"currentMetrics"`
	Conditions         []HorizontalPodAutoscalerCondition `json:"conditions,omitempty"`
}

// MetricStatus is the current value of one metric.
type MetricStatus struct {
	Type              string                         `json:"type"`
	Resource          *ResourceMetricStatus          `json:"resource,omitempty"`
	ContainerResource *ContainerResourceMetricStatus `json:"containerResource,omitempty"`
}

// ResourceMetricStatus is the current value of a resource metric.
type ResourceMetricStatus struct {
	Name    string            `json:"name"`
	Current MetricValueStatus `json:"current"`
}

// ContainerResourceMetricStatus is the current value of a container
// resource metric.
type ContainerResourceMetricStatus struct {
	Name      string            `json:"name"`
	Container string            `json:"container"`
	Current   MetricValueStatus `json:"current"`
}

// MetricValueStatus holds the current value.
type MetricValueStatus struct {
	Value              *resource.Quantity `json:"value,omitempty"`
	AverageValue       *resource.Quantity `json:"averageValue,omitempty"`
	AverageUtilization *int32             `json:"averageUtilization,omitempty"`
}

// Condition types.
const (
	ScalingActive  = "ScalingActive"
	AbleToScale    = "AbleToScale"
	ScalingLimited = "ScalingLimited"
)

// HorizontalPodAutoscalerCondition is one aspect of the autoscaler's state.
type HorizontalPodAutoscalerCondition struct {
	Type               string      `json:"type"`
	Status             string      `json:"status"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitzero"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
}

// Scale is a workload's replica count as the scale subresource shows it.
type Scale struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              ScaleSpec   `json:"spec"`
	Status            ScaleStatus `json:"status"`
}

// ScaleSpec is the desired replica count.
type ScaleSpec struct {
	Replicas int32 `json:"replicas"`
}

// ScaleStatus is the current replica count and the Pod selector.
type ScaleStatus struct {
	Replicas int32  `json:"replicas"`
	Selector string `json:"selector,omitempty"`
}

// SetDefaultsHorizontalPodAutoscaler fills omitted fields as Kubernetes
// does: one replica at least, CPU at 80%, and the default rules of a
// behavior that is given.
func SetDefaultsHorizontalPodAutoscaler(h *HorizontalPodAutoscaler) {
	if h.Spec.MinReplicas == nil {
		h.Spec.MinReplicas = ptr.To[int32](1)
	}
	if len(h.Spec.Metrics) == 0 {
		h.Spec.Metrics = []MetricSpec{{Type: ResourceMetricSourceType, Resource: &ResourceMetricSource{Name: "cpu",
			Target: MetricTarget{Type: UtilizationMetricType, AverageUtilization: ptr.To[int32](80)}}}}
	}
	if b := h.Spec.Behavior; b != nil {
		b.ScaleUp = defaultRules(b.ScaleUp, DefaultScaleUpRules())
		b.ScaleDown = defaultRules(b.ScaleDown, DefaultScaleDownRules())
	}
}

// DefaultScaleUpRules: no stabilization; 4 Pods or 100% per 15s,
// whichever is more.
func DefaultScaleUpRules() *HPAScalingRules {
	return &HPAScalingRules{StabilizationWindowSeconds: ptr.To[int32](0), SelectPolicy: ptr.To(MaxChangePolicySelect),
		Policies: []HPAScalingPolicy{{Type: PodsScalingPolicy, Value: 4, PeriodSeconds: 15},
			{Type: PercentScalingPolicy, Value: 100, PeriodSeconds: 15}}}
}

// DefaultScaleDownRules: the highest recommendation of the last 5
// minutes; down to any count per 15s.
func DefaultScaleDownRules() *HPAScalingRules {
	return &HPAScalingRules{StabilizationWindowSeconds: ptr.To[int32](300), SelectPolicy: ptr.To(MaxChangePolicySelect),
		Policies: []HPAScalingPolicy{{Type: PercentScalingPolicy, Value: 100, PeriodSeconds: 15}}}
}

func defaultRules(r, def *HPAScalingRules) *HPAScalingRules {
	if r == nil {
		return def
	}
	if r.StabilizationWindowSeconds == nil {
		r.StabilizationWindowSeconds = def.StabilizationWindowSeconds
	}
	if r.SelectPolicy == nil {
		r.SelectPolicy = def.SelectPolicy
	}
	if len(r.Policies) == 0 {
		r.Policies = def.Policies
	}
	return r
}

// Condition returns the condition of the given type, if present.
func (s *HorizontalPodAutoscalerStatus) Condition(t string) *HorizontalPodAutoscalerCondition {
	for i := range s.Conditions {
		if s.Conditions[i].Type == t {
			return &s.Conditions[i]
		}
	}
	return nil
}
