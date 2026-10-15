// Package v1 contains the storage.k8s.io/v1 types (storagev1):
// StorageClass, which names a provisioner of PersistentVolumes.
package v1

import (
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

// StorageClass describes a kind of storage that claims can ask for and
// that its provisioner creates volumes of.
type StorageClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Provisioner       string            `json:"provisioner"`
	Parameters        map[string]string `json:"parameters,omitempty"`
	// ReclaimPolicy of the provisioned volumes: Delete (default) or Retain.
	ReclaimPolicy *string  `json:"reclaimPolicy,omitempty"`
	MountOptions  []string `json:"mountOptions,omitempty"`
	// AllowVolumeExpansion is shown but not acted on by mini-k8s.
	AllowVolumeExpansion *bool `json:"allowVolumeExpansion,omitempty"`
	// VolumeBindingMode is Immediate (default) or WaitForFirstConsumer.
	VolumeBindingMode *string `json:"volumeBindingMode,omitempty"`
}

// Binding modes, and the annotation marking the default class.
const (
	VolumeBindingImmediate            = "Immediate"
	VolumeBindingWaitForFirstConsumer = "WaitForFirstConsumer"
	AnnDefaultStorageClass            = "storageclass.kubernetes.io/is-default-class"
)

// IsDefault reports whether the class is the cluster's default.
func IsDefault(sc *StorageClass) bool { return sc.Annotations[AnnDefaultStorageClass] == "true" }

// WaitsForConsumer reports whether binding waits for a Pod to be scheduled.
func WaitsForConsumer(sc *StorageClass) bool {
	return sc.VolumeBindingMode != nil && *sc.VolumeBindingMode == VolumeBindingWaitForFirstConsumer
}

// SetDefaults fills the reclaim policy and binding mode.
func SetDefaults(sc *StorageClass) {
	if sc.ReclaimPolicy == nil {
		p := "Delete"
		sc.ReclaimPolicy = &p
	}
	if sc.VolumeBindingMode == nil {
		m := VolumeBindingImmediate
		sc.VolumeBindingMode = &m
	}
}
