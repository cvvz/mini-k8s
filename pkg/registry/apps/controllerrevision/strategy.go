// Package controllerrevision holds the API strategy for ControllerRevisions.
package controllerrevision

import (
	appsv1 "mini-k8s/pkg/apis/apps/v1"
	"mini-k8s/pkg/apis/apps/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy validates ControllerRevisions, whose data is immutable (only
// the revision number changes, when a rollback reuses a revision).
var Strategy = apiserver.Strategy{
	Validate: func(obj metav1.Object) []string {
		return validation.ValidateControllerRevision(obj.(*appsv1.ControllerRevision))
	},
	ValidateUpdate: func(obj, old metav1.Object) []string {
		return validation.ValidateControllerRevisionUpdate(obj.(*appsv1.ControllerRevision), old.(*appsv1.ControllerRevision))
	},
}
