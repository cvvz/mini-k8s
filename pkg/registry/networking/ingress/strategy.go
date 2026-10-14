// Package ingress holds the API strategy for Ingresses.
package ingress

import (
	metav1 "mini-k8s/pkg/apis/meta/v1"
	networkingv1 "mini-k8s/pkg/apis/networking/v1"
	"mini-k8s/pkg/apis/networking/validation"
	"mini-k8s/pkg/apiserver"
)

// Strategy validates Ingresses.
var Strategy = apiserver.Strategy{
	Validate: func(obj metav1.Object) []string { return validation.ValidateIngress(obj.(*networkingv1.Ingress)) },
}
