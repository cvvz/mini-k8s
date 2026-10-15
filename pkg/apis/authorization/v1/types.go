// Package v1 contains the authorization.k8s.io/v1 types
// (authorizationv1): SelfSubjectAccessReview asks whether the requester
// may do something (`kk auth can-i`). It is not stored.
package v1

import metav1 "mini-k8s/pkg/apis/meta/v1"

// SelfSubjectAccessReview checks one action of the requester.
type SelfSubjectAccessReview struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              SelfSubjectAccessReviewSpec `json:"spec"`
	Status            SubjectAccessReviewStatus   `json:"status,omitzero"`
}

// SelfSubjectAccessReviewSpec names a resource or a non-resource action.
type SelfSubjectAccessReviewSpec struct {
	ResourceAttributes    *ResourceAttributes    `json:"resourceAttributes,omitempty"`
	NonResourceAttributes *NonResourceAttributes `json:"nonResourceAttributes,omitempty"`
}

// ResourceAttributes describe an action on an API resource.
type ResourceAttributes struct {
	Namespace   string `json:"namespace,omitempty"`
	Verb        string `json:"verb,omitempty"`
	Group       string `json:"group,omitempty"`
	Version     string `json:"version,omitempty"`
	Resource    string `json:"resource,omitempty"`
	Subresource string `json:"subresource,omitempty"`
	Name        string `json:"name,omitempty"`
}

// NonResourceAttributes describe a request to a path such as /healthz.
type NonResourceAttributes struct {
	Path string `json:"path,omitempty"`
	Verb string `json:"verb,omitempty"`
}

// SubjectAccessReviewStatus is the decision.
type SubjectAccessReviewStatus struct {
	Allowed         bool   `json:"allowed"`
	Denied          bool   `json:"denied,omitempty"`
	Reason          string `json:"reason,omitempty"`
	EvaluationError string `json:"evaluationError,omitempty"`
}
