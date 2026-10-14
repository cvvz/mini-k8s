// Package deployment holds the API strategy for Deployments.
package deployment

import (
	appsv1 "mini-k8s/pkg/apis/apps/v1"
	"mini-k8s/pkg/apis/apps/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy defaults and validates Deployments; the selector is immutable.
var Strategy = apiserver.Strategy{
	Defaults: func(obj metav1.Object) { appsv1.SetDefaultsDeployment(obj.(*appsv1.Deployment)) },
	Validate: func(obj metav1.Object) []string { return validation.ValidateDeployment(obj.(*appsv1.Deployment)) },
	ValidateUpdate: func(obj, old metav1.Object) []string {
		return validation.ValidateDeploymentUpdate(obj.(*appsv1.Deployment), old.(*appsv1.Deployment))
	},
}
