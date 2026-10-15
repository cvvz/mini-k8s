// Package v1 contains the authentication.k8s.io/v1 types
// (authenticationv1): TokenRequest, which issues service account tokens,
// and SelfSubjectReview, which tells a client who it is. Neither is stored.
package v1

import metav1 "mini-k8s/pkg/apis/meta/v1"

// UserInfo is an authenticated identity.
type UserInfo struct {
	Username string              `json:"username,omitempty"`
	UID      string              `json:"uid,omitempty"`
	Groups   []string            `json:"groups,omitempty"`
	Extra    map[string][]string `json:"extra,omitempty"`
}

// TokenRequest asks for a token of a service account, optionally bound
// to an object (a Pod) whose deletion invalidates it.
type TokenRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              TokenRequestSpec   `json:"spec"`
	Status            TokenRequestStatus `json:"status,omitzero"`
}

// TokenRequestSpec is the requested audience, lifetime and binding.
type TokenRequestSpec struct {
	Audiences         []string              `json:"audiences"`
	ExpirationSeconds *int64                `json:"expirationSeconds,omitempty"`
	BoundObjectRef    *BoundObjectReference `json:"boundObjectRef,omitempty"`
}

// BoundObjectReference is the object a token lives as long as.
type BoundObjectReference struct {
	Kind       string `json:"kind,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
	Name       string `json:"name,omitempty"`
	UID        string `json:"uid,omitempty"`
}

// TokenRequestStatus carries the issued token.
type TokenRequestStatus struct {
	Token               string      `json:"token"`
	ExpirationTimestamp metav1.Time `json:"expirationTimestamp"`
}

// SelfSubjectReview reports the requester's identity.
type SelfSubjectReview struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Status            SelfSubjectReviewStatus `json:"status,omitzero"`
}

// SelfSubjectReviewStatus holds the identity.
type SelfSubjectReviewStatus struct {
	UserInfo UserInfo `json:"userInfo,omitzero"`
}
