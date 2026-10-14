// Package endpoints holds the API strategy for Endpoints.
package endpoints

import (
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/apis/core/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy defaults and validates Endpoints.
var Strategy = apiserver.Strategy{
	Defaults: func(obj metav1.Object) { corev1.SetDefaultsEndpoints(obj.(*corev1.Endpoints)) },
	Validate: func(obj metav1.Object) []string { return validation.ValidateEndpoints(obj.(*corev1.Endpoints)) },
}
