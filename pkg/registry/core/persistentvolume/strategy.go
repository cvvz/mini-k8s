// Package persistentvolume holds the API strategy for PersistentVolumes.
package persistentvolume

import (
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/apis/core/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
	"mini-k8s/pkg/util/ptr"
)

// Strategy defaults the volume mode and the reclaim policy (Retain), starts
// new volumes Pending with the pv-protection finalizer (the
// StorageObjectInUseProtection admission plugin) and keeps their source.
var Strategy = apiserver.Strategy{
	Defaults: func(obj metav1.Object) {
		pv := obj.(*corev1.PersistentVolume)
		if pv.Spec.VolumeMode == nil {
			pv.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeFilesystem)
		}
		if pv.Spec.PersistentVolumeReclaimPolicy == "" {
			pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
		}
	},
	PrepareForCreate: func(obj metav1.Object) {
		pv := obj.(*corev1.PersistentVolume)
		pv.Status.Phase = corev1.VolumePending
		if !pv.HasFinalizer(corev1.PVProtectionFinalizer) {
			pv.Finalizers = append(pv.Finalizers, corev1.PVProtectionFinalizer)
		}
	},
	Validate: func(obj metav1.Object) []string {
		return validation.ValidatePersistentVolume(obj.(*corev1.PersistentVolume))
	},
	ValidateUpdate: func(obj, old metav1.Object) []string {
		return validation.ValidatePersistentVolumeUpdate(obj.(*corev1.PersistentVolume), old.(*corev1.PersistentVolume))
	},
}
