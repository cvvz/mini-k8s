// Package event holds the API strategy for Events.
package event

import (
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/apis/core/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy defaults counts and timestamps and exposes the involved object
// as selectable fields.
var Strategy = apiserver.Strategy{
	Defaults: func(obj metav1.Object) {
		e := obj.(*corev1.Event)
		if e.Count == 0 {
			e.Count = 1
		}
		if e.Type == "" {
			e.Type = corev1.EventTypeNormal
		}
		if e.FirstTimestamp.IsZero() {
			e.FirstTimestamp = metav1.Now()
		}
		if e.LastTimestamp.IsZero() {
			e.LastTimestamp = e.FirstTimestamp
		}
	},
	Validate: func(obj metav1.Object) []string { return validation.ValidateEvent(obj.(*corev1.Event)) },
	Fields: func(obj metav1.Object) map[string]string {
		e := obj.(*corev1.Event)
		return map[string]string{
			"involvedObject.kind": e.InvolvedObject.Kind, "involvedObject.name": e.InvolvedObject.Name,
			"involvedObject.namespace": e.InvolvedObject.Namespace, "involvedObject.uid": e.InvolvedObject.UID,
			"reason": e.Reason, "type": e.Type,
		}
	},
}
