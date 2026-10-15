// Package persistentvolumeclaim holds the API strategy for
// PersistentVolumeClaims.
package persistentvolumeclaim

import (
	"context"
	"encoding/json"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/apis/core/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	storagev1 "mini-k8s/pkg/apis/storage/v1"
	"mini-k8s/pkg/apiserver"
	"mini-k8s/pkg/storage/etcd3"
	"mini-k8s/pkg/util/ptr"
)

// Lister lists stored objects under a key prefix (the API server's store).
type Lister interface {
	List(ctx context.Context, prefix string) ([]etcd3.KV, int64, error)
}

// NewStrategy defaults the volume mode, gives claims without a class the
// default StorageClass (the DefaultStorageClass admission plugin, reading
// the classes stored under prefix), starts them Pending with the
// pvc-protection finalizer and keeps their spec.
func NewStrategy(store Lister, prefix string) apiserver.Strategy {
	return apiserver.Strategy{
		Defaults: func(obj metav1.Object) {
			pvc := obj.(*corev1.PersistentVolumeClaim)
			if pvc.Spec.VolumeMode == nil {
				pvc.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeFilesystem)
			}
		},
		PrepareForCreate: func(obj metav1.Object) {
			pvc := obj.(*corev1.PersistentVolumeClaim)
			pvc.Status.Phase = corev1.ClaimPending
			if !pvc.HasFinalizer(corev1.PVCProtectionFinalizer) {
				pvc.Finalizers = append(pvc.Finalizers, corev1.PVCProtectionFinalizer)
			}
		},
		BeginCreate: func(ctx context.Context, obj metav1.Object, _ bool) (apiserver.FinishFunc, error) {
			pvc := obj.(*corev1.PersistentVolumeClaim)
			if pvc.Spec.StorageClassName == nil {
				name, err := defaultClass(ctx, store, prefix)
				if err != nil {
					return nil, err
				}
				if name != "" {
					pvc.Spec.StorageClassName = &name
				}
			}
			return func(context.Context, bool) {}, nil
		},
		Validate: func(obj metav1.Object) []string {
			return validation.ValidatePersistentVolumeClaim(obj.(*corev1.PersistentVolumeClaim))
		},
		ValidateUpdate: func(obj, old metav1.Object) []string {
			return validation.ValidatePersistentVolumeClaimUpdate(obj.(*corev1.PersistentVolumeClaim), old.(*corev1.PersistentVolumeClaim))
		},
	}
}

// defaultClass returns the default StorageClass; with several, the newest
// wins, as in Kubernetes.
func defaultClass(ctx context.Context, store Lister, prefix string) (string, error) {
	kvs, _, err := store.List(ctx, prefix+"/storageclasses/")
	if err != nil {
		return "", err
	}
	var best *storagev1.StorageClass
	for _, kv := range kvs {
		var sc storagev1.StorageClass
		if json.Unmarshal(kv.Value, &sc) != nil || !storagev1.IsDefault(&sc) {
			continue
		}
		created, bestCreated := sc.CreationTimestamp.Time, time.Time{}
		if best != nil {
			bestCreated = best.CreationTimestamp.Time
		}
		if best == nil || created.After(bestCreated) || (created.Equal(bestCreated) && sc.Name < best.Name) {
			best = &sc
		}
	}
	if best == nil {
		return "", nil
	}
	return best.Name, nil
}
