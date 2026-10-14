// Package service holds the API strategy for Services.
package service

import (
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/apis/core/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy defaults and validates Services.
var Strategy = apiserver.Strategy{
	NameIsDNSLabel: true,
	Defaults:       func(obj metav1.Object) { corev1.SetDefaultsServiceSpec(&obj.(*corev1.Service).Spec) },
	Validate:       func(obj metav1.Object) []string { return validation.ValidateServiceSpec(&obj.(*corev1.Service).Spec) },
}
