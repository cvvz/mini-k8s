// Package horizontalpodautoscaler holds the API strategy for
// HorizontalPodAutoscalers.
package horizontalpodautoscaler

import (
	autoscalingv2 "mini-k8s/pkg/apis/autoscaling/v2"
	"mini-k8s/pkg/apis/autoscaling/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy defaults and validates HorizontalPodAutoscalers.
var Strategy = apiserver.Strategy{
	Defaults: func(obj metav1.Object) {
		autoscalingv2.SetDefaultsHorizontalPodAutoscaler(obj.(*autoscalingv2.HorizontalPodAutoscaler))
	},
	Validate: func(obj metav1.Object) []string {
		return validation.ValidateHorizontalPodAutoscaler(obj.(*autoscalingv2.HorizontalPodAutoscaler))
	},
}
