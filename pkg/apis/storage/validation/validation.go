// Package validation checks storage.k8s.io objects like Kubernetes.
package validation

import (
	"fmt"
	"reflect"

	storagev1 "mini-k8s/pkg/apis/storage/v1"
	"mini-k8s/pkg/labels"
)

// ValidateStorageClass requires a provisioner and known policies.
func ValidateStorageClass(sc *storagev1.StorageClass) []string {
	var errs []string
	if sc.Provisioner == "" {
		errs = append(errs, "provisioner: Required value")
	} else if labels.ValidateLabels(map[string]string{sc.Provisioner: ""}) != nil {
		errs = append(errs, fmt.Sprintf("provisioner: Invalid value: %q: must be a qualified name", sc.Provisioner))
	}
	if p := sc.ReclaimPolicy; p != nil && *p != "Delete" && *p != "Retain" {
		errs = append(errs, fmt.Sprintf(`reclaimPolicy: Unsupported value: %q: supported values: "Delete", "Retain"`, *p))
	}
	if m := sc.VolumeBindingMode; m != nil && *m != storagev1.VolumeBindingImmediate && *m != storagev1.VolumeBindingWaitForFirstConsumer {
		errs = append(errs, fmt.Sprintf(`volumeBindingMode: Unsupported value: %q: supported values: "Immediate", "WaitForFirstConsumer"`, *m))
	}
	return errs
}

// ValidateStorageClassUpdate forbids changing what volumes were made with.
func ValidateStorageClassUpdate(sc, old *storagev1.StorageClass) []string {
	var errs []string
	if !reflect.DeepEqual(sc.Parameters, old.Parameters) {
		errs = append(errs, "parameters: Forbidden: updates to parameters are forbidden.")
	}
	if sc.Provisioner != old.Provisioner {
		errs = append(errs, "provisioner: Forbidden: updates to provisioner are forbidden.")
	}
	if !reflect.DeepEqual(sc.ReclaimPolicy, old.ReclaimPolicy) {
		errs = append(errs, "reclaimPolicy: Forbidden: updates to reclaimPolicy are forbidden.")
	}
	if !reflect.DeepEqual(sc.VolumeBindingMode, old.VolumeBindingMode) {
		errs = append(errs, "volumeBindingMode: Forbidden: field is immutable")
	}
	return errs
}
