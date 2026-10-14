// Package deployment holds the API strategy for Deployments.
package deployment

import (
	appsv1 "mini-k8s/pkg/apis/apps/v1"
	"mini-k8s/pkg/apis/apps/validation"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
	"mini-k8s/pkg/util/ptr"
)

// Strategy defaults and validates Deployments; the selector is immutable.
var Strategy = apiserver.Strategy{
	Defaults: func(obj metav1.Object) {
		d := obj.(*appsv1.Deployment)
		if d.Spec.Replicas == nil {
			d.Spec.Replicas = ptr.To[int32](1)
		}
		corev1.SetDefaultsPodSpec(&d.Spec.Template.Spec)
	},
	Validate: func(obj metav1.Object) []string { return validation.ValidateDeployment(obj.(*appsv1.Deployment)) },
	ValidateUpdate: func(obj, old metav1.Object) []string {
		return validation.ValidateDeploymentUpdate(obj.(*appsv1.Deployment), old.(*appsv1.Deployment))
	},
}
