package v1

import metav1 "mini-k8s/pkg/apis/meta/v1"

// ServiceAccount is an identity for processes in Pods; its tokens are
// requested from the API server (TokenRequest) and projected into Pods.
type ServiceAccount struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Secrets           []ObjectReference `json:"secrets,omitempty"`
	// AutomountServiceAccountToken false keeps the token out of Pods that
	// do not ask for it.
	AutomountServiceAccountToken *bool `json:"automountServiceAccountToken,omitempty"`
}

// Service account names and paths.
const (
	DefaultServiceAccountName = "default"
	// ServiceAccountVolumePrefix starts the name of the token volume.
	ServiceAccountVolumePrefix = "kube-api-access-"
	// ServiceAccountMountPath is where containers find token and namespace.
	ServiceAccountMountPath = "/var/run/secrets/kubernetes.io/serviceaccount"
	// ServiceAccountTokenExpirationSeconds is the lifetime of projected
	// tokens, as the ServiceAccount admission plugin sets it.
	ServiceAccountTokenExpirationSeconds = 3607
)
