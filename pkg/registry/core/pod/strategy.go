// Package pod holds the API strategy for Pods.
package pod

import (
	"reflect"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/apis/core/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy defaults and validates Pods; new Pods start Pending and the spec
// is immutable (nodeName is set through the binding subresource).
var Strategy = apiserver.Strategy{
	Defaults:         func(obj metav1.Object) { corev1.SetDefaultsPodSpec(&obj.(*corev1.Pod).Spec) },
	PrepareForCreate: func(obj metav1.Object) { obj.(*corev1.Pod).Status.Phase = corev1.PodPending },
	Validate:         func(obj metav1.Object) []string { return validation.ValidatePodSpec("spec", &obj.(*corev1.Pod).Spec) },
	ValidateUpdate: func(obj, old metav1.Object) []string {
		if !reflect.DeepEqual(obj.(*corev1.Pod).Spec, old.(*corev1.Pod).Spec) {
			return []string{"spec: Forbidden: pod spec is immutable after creation"}
		}
		return nil
	},
	Fields: func(obj metav1.Object) map[string]string {
		p := obj.(*corev1.Pod)
		return map[string]string{"spec.nodeName": p.Spec.NodeName, "status.phase": p.Status.Phase}
	},
}
