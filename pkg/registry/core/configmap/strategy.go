// Package configmap holds the API strategy for ConfigMaps.
package configmap

import (
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/apis/core/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy validates ConfigMaps and keeps immutable ones unchanged.
var Strategy = apiserver.Strategy{
	Validate: func(obj metav1.Object) []string { return validation.ValidateConfigMap(obj.(*corev1.ConfigMap)) },
	ValidateUpdate: func(obj, old metav1.Object) []string {
		return validation.ValidateConfigMapUpdate(obj.(*corev1.ConfigMap), old.(*corev1.ConfigMap))
	},
}
