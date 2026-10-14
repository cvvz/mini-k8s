// Package namespace holds the API strategy for Namespaces.
package namespace

import (
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy makes new namespaces Active and holds them with the
// "kubernetes" finalizer until the namespace controller empties them.
var Strategy = apiserver.Strategy{
	NameIsDNSLabel: true,
	PrepareForCreate: func(obj metav1.Object) {
		ns := obj.(*corev1.Namespace)
		ns.Status.Phase = corev1.NamespaceActive
		if !ns.HasFinalizer(corev1.FinalizerKubernetes) {
			ns.Finalizers = append(ns.Finalizers, corev1.FinalizerKubernetes)
		}
	},
	Fields: func(obj metav1.Object) map[string]string {
		return map[string]string{"status.phase": obj.(*corev1.Namespace).Status.Phase}
	},
}
