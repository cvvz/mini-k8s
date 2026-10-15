// Package pod holds the API strategy for Pods.
package pod

import (
	"reflect"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/apis/core/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
	"mini-k8s/pkg/util/deepcopy"
)

// Strategy defaults and validates Pods; new Pods start Pending with their QoS
// class and the spec is immutable (nodeName is set through the binding subresource).
var Strategy = apiserver.Strategy{
	Defaults: func(obj metav1.Object) {
		spec := &obj.(*corev1.Pod).Spec
		corev1.SetDefaultsPodSpec(spec)
		// The DefaultTolerationSeconds admission plugin; on updates too, so a
		// re-applied manifest that lists its own tolerations keeps them.
		corev1.AddDefaultTolerations(spec)
	},
	PrepareForCreate: func(obj metav1.Object) {
		pod := obj.(*corev1.Pod)
		pod.Status = corev1.PodStatus{Phase: corev1.PodPending, QOSClass: corev1.GetPodQOS(&pod.Spec)}
	},
	Validate: func(obj metav1.Object) []string { return validation.ValidatePodSpec("spec", &obj.(*corev1.Pod).Spec) },
	ValidateUpdate: func(obj, old metav1.Object) []string {
		// Pods stored before a field gained a default compare as defaulted.
		oldSpec := deepcopy.Copy(&old.(*corev1.Pod).Spec)
		corev1.SetDefaultsPodSpec(oldSpec)
		corev1.AddDefaultTolerations(oldSpec)
		if !reflect.DeepEqual(obj.(*corev1.Pod).Spec, *oldSpec) {
			return []string{"spec: Forbidden: pod spec is immutable after creation"}
		}
		return nil
	},
	Fields: func(obj metav1.Object) map[string]string {
		p := obj.(*corev1.Pod)
		return map[string]string{"spec.nodeName": p.Spec.NodeName, "status.phase": p.Status.Phase}
	},
}
