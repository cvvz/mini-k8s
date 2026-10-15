package v1

import (
	"mini-k8s/pkg/api/resource"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

// ConfigMap holds configuration for Pods: strings in data, bytes in
// binaryData.
type ConfigMap struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	// Immutable, once true, forbids changing the data (and unsetting it).
	Immutable  *bool             `json:"immutable,omitempty"`
	Data       map[string]string `json:"data,omitempty"`
	BinaryData map[string][]byte `json:"binaryData,omitempty"`
}

// Secret holds sensitive data, base64-encoded in JSON.
type Secret struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Immutable         *bool             `json:"immutable,omitempty"`
	Data              map[string][]byte `json:"data,omitempty"`
	// StringData is a write-only convenience: the API server merges it into
	// data (overriding the same keys) and never stores it.
	StringData map[string]string `json:"stringData,omitempty"`
	// Type is Opaque by default and immutable.
	Type string `json:"type,omitempty"`
}

// Secret types whose keys the API server checks.
const (
	SecretTypeOpaque           = "Opaque"
	SecretTypeTLS              = "kubernetes.io/tls"
	SecretTypeBasicAuth        = "kubernetes.io/basic-auth"
	SecretTypeSSHAuth          = "kubernetes.io/ssh-auth"
	SecretTypeDockerConfigJSON = "kubernetes.io/dockerconfigjson"

	TLSCertKey           = "tls.crt"
	TLSPrivateKeyKey     = "tls.key"
	BasicAuthUsernameKey = "username"
	BasicAuthPasswordKey = "password"
	SSHAuthPrivateKey    = "ssh-privatekey"
	DockerConfigJSONKey  = ".dockerconfigjson"
)

// MaxConfigDataSize bounds the data of a ConfigMap or Secret (1 MiB).
const MaxConfigDataSize = 1 << 20

// EnvVarSource takes an environment variable's value from elsewhere;
// exactly one member is set.
type EnvVarSource struct {
	FieldRef         *ObjectFieldSelector   `json:"fieldRef,omitempty"`
	ResourceFieldRef *ResourceFieldSelector `json:"resourceFieldRef,omitempty"`
	ConfigMapKeyRef  *ConfigMapKeySelector  `json:"configMapKeyRef,omitempty"`
	SecretKeyRef     *SecretKeySelector     `json:"secretKeyRef,omitempty"`
}

// ObjectFieldSelector selects a field of the Pod, such as metadata.name or
// metadata.labels['app'].
type ObjectFieldSelector struct {
	APIVersion string `json:"apiVersion,omitempty"`
	FieldPath  string `json:"fieldPath"`
}

// ResourceFieldSelector selects a container's request or limit, such as
// limits.cpu, scaled by divisor (1 by default).
type ResourceFieldSelector struct {
	ContainerName string             `json:"containerName,omitempty"`
	Resource      string             `json:"resource"`
	Divisor       *resource.Quantity `json:"divisor,omitempty"`
}

// LocalObjectReference names an object in the Pod's namespace.
type LocalObjectReference struct {
	Name string `json:"name"`
}

// ConfigMapKeySelector selects a key of a ConfigMap.
type ConfigMapKeySelector struct {
	LocalObjectReference `json:",inline"`
	Key                  string `json:"key"`
	// Optional allows the ConfigMap or the key to be missing.
	Optional *bool `json:"optional,omitempty"`
}

// SecretKeySelector selects a key of a Secret.
type SecretKeySelector struct {
	LocalObjectReference `json:",inline"`
	Key                  string `json:"key"`
	Optional             *bool  `json:"optional,omitempty"`
}

// EnvFromSource turns every key of a ConfigMap or Secret into a variable,
// with an optional prefix.
type EnvFromSource struct {
	Prefix       string              `json:"prefix,omitempty"`
	ConfigMapRef *ConfigMapEnvSource `json:"configMapRef,omitempty"`
	SecretRef    *SecretEnvSource    `json:"secretRef,omitempty"`
}

// ConfigMapEnvSource names the ConfigMap of an envFrom entry.
type ConfigMapEnvSource struct {
	LocalObjectReference `json:",inline"`
	Optional             *bool `json:"optional,omitempty"`
}

// SecretEnvSource names the Secret of an envFrom entry.
type SecretEnvSource struct {
	LocalObjectReference `json:",inline"`
	Optional             *bool `json:"optional,omitempty"`
}

// KeyToPath projects a key to a relative file path, with an optional mode.
type KeyToPath struct {
	Key  string `json:"key"`
	Path string `json:"path"`
	Mode *int32 `json:"mode,omitempty"`
}

// ConfigMapVolumeSource puts a ConfigMap's keys (or the listed items) into
// files.
type ConfigMapVolumeSource struct {
	LocalObjectReference `json:",inline"`
	Items                []KeyToPath `json:"items,omitempty"`
	// DefaultMode is the files' mode (0644 by default).
	DefaultMode *int32 `json:"defaultMode,omitempty"`
	Optional    *bool  `json:"optional,omitempty"`
}

// SecretVolumeSource puts a Secret's keys (or the listed items) into files
// on a tmpfs.
type SecretVolumeSource struct {
	SecretName  string      `json:"secretName,omitempty"`
	Items       []KeyToPath `json:"items,omitempty"`
	DefaultMode *int32      `json:"defaultMode,omitempty"`
	Optional    *bool       `json:"optional,omitempty"`
}

// DownwardAPIVolumeSource writes Pod fields and container resources into
// files.
type DownwardAPIVolumeSource struct {
	Items       []DownwardAPIVolumeFile `json:"items,omitempty"`
	DefaultMode *int32                  `json:"defaultMode,omitempty"`
}

// DownwardAPIVolumeFile is one file of a downward API volume.
type DownwardAPIVolumeFile struct {
	Path             string                 `json:"path"`
	FieldRef         *ObjectFieldSelector   `json:"fieldRef,omitempty"`
	ResourceFieldRef *ResourceFieldSelector `json:"resourceFieldRef,omitempty"`
	Mode             *int32                 `json:"mode,omitempty"`
}

// ProjectedVolumeSource combines ConfigMaps, Secrets and downward API
// fields in one directory.
type ProjectedVolumeSource struct {
	Sources     []VolumeProjection `json:"sources"`
	DefaultMode *int32             `json:"defaultMode,omitempty"`
}

// VolumeProjection is one source of a projected volume.
type VolumeProjection struct {
	ConfigMap   *ConfigMapProjection   `json:"configMap,omitempty"`
	Secret      *SecretProjection      `json:"secret,omitempty"`
	DownwardAPI *DownwardAPIProjection `json:"downwardAPI,omitempty"`
	// ServiceAccountToken projects a token of the Pod's service account,
	// requested by the kubelet and renewed before it expires.
	ServiceAccountToken *ServiceAccountTokenProjection `json:"serviceAccountToken,omitempty"`
}

// ServiceAccountTokenProjection is a service account token file.
type ServiceAccountTokenProjection struct {
	Audience          string `json:"audience,omitempty"`
	ExpirationSeconds *int64 `json:"expirationSeconds,omitempty"`
	Path              string `json:"path"`
}

// ConfigMapProjection projects a ConfigMap.
type ConfigMapProjection struct {
	LocalObjectReference `json:",inline"`
	Items                []KeyToPath `json:"items,omitempty"`
	Optional             *bool       `json:"optional,omitempty"`
}

// SecretProjection projects a Secret.
type SecretProjection struct {
	LocalObjectReference `json:",inline"`
	Items                []KeyToPath `json:"items,omitempty"`
	Optional             *bool       `json:"optional,omitempty"`
}

// DownwardAPIProjection projects downward API fields.
type DownwardAPIProjection struct {
	Items []DownwardAPIVolumeFile `json:"items,omitempty"`
}

// ProjectedDefaultMode is the default file mode of configMap, secret,
// downwardAPI and projected volumes.
const ProjectedDefaultMode int32 = 0o644
