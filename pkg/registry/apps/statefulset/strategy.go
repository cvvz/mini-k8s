// Package statefulset holds the API strategy for StatefulSets.
package statefulset

import (
	appsv1 "mini-k8s/pkg/apis/apps/v1"
	"mini-k8s/pkg/apis/apps/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy defaults and validates StatefulSets; after creation only the
// fields Kubernetes allows may change.
var Strategy = apiserver.Strategy{
	Defaults: func(obj metav1.Object) { appsv1.SetDefaultsStatefulSet(obj.(*appsv1.StatefulSet)) },
	Validate: func(obj metav1.Object) []string { return validation.ValidateStatefulSet(obj.(*appsv1.StatefulSet)) },
	ValidateUpdate: func(obj, old metav1.Object) []string {
		return validation.ValidateStatefulSetUpdate(obj.(*appsv1.StatefulSet), old.(*appsv1.StatefulSet))
	},
}
