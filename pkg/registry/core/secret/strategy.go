// Package secret holds the API strategy for Secrets.
package secret

import (
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/apis/core/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy defaults the type to Opaque, merges stringData into data (it is
// never stored), validates and keeps the type and immutable Secrets
// unchanged. Secrets can be selected by type.
var Strategy = apiserver.Strategy{
	Defaults: func(obj metav1.Object) {
		s := obj.(*corev1.Secret)
		if s.Type == "" {
			s.Type = corev1.SecretTypeOpaque
		}
		for k, v := range s.StringData {
			if s.Data == nil {
				s.Data = map[string][]byte{}
			}
			s.Data[k] = []byte(v)
		}
		s.StringData = nil
	},
	Validate: func(obj metav1.Object) []string { return validation.ValidateSecret(obj.(*corev1.Secret)) },
	ValidateUpdate: func(obj, old metav1.Object) []string {
		return validation.ValidateSecretUpdate(obj.(*corev1.Secret), old.(*corev1.Secret))
	},
	Fields: func(obj metav1.Object) map[string]string { return map[string]string{"type": obj.(*corev1.Secret).Type} },
}
