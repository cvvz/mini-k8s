// Package replicaset holds the API strategy for ReplicaSets.
package replicaset

import (
	appsv1 "mini-k8s/pkg/apis/apps/v1"
	"mini-k8s/pkg/apis/apps/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy defaults and validates ReplicaSets; the selector is immutable.
var Strategy = apiserver.Strategy{
	Defaults: func(obj metav1.Object) { appsv1.SetDefaultsReplicaSet(obj.(*appsv1.ReplicaSet)) },
	Validate: func(obj metav1.Object) []string { return validation.ValidateReplicaSet(obj.(*appsv1.ReplicaSet)) },
	ValidateUpdate: func(obj, old metav1.Object) []string {
		return validation.ValidateReplicaSetUpdate(obj.(*appsv1.ReplicaSet), old.(*appsv1.ReplicaSet))
	},
}
