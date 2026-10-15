package validation

import (
	"fmt"
	"reflect"
	"strings"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/util/validation"
)

var accessModes = []string{corev1.ReadOnlyMany, corev1.ReadWriteMany, corev1.ReadWriteOnce, corev1.ReadWriteOncePod}

func validateAccessModes(path string, modes []string) []string {
	if len(modes) == 0 {
		return []string{path + ": Required value: at least 1 access mode is required"}
	}
	var errs []string
	for _, m := range modes {
		known := false
		for _, a := range accessModes {
			known = known || m == a
		}
		if !known {
			errs = append(errs, notSupported(path, m, accessModes))
		}
		if m == corev1.ReadWriteOncePod && len(modes) > 1 {
			errs = append(errs, invalid(path, fmt.Sprint(modes), "may not use ReadWriteOncePod with other access modes"))
		}
	}
	return errs
}

func validateStorageAmount(path string, list corev1.ResourceList) []string {
	q, ok := list[corev1.ResourceStorage]
	if !ok {
		return []string{path + "[storage]: Required value"}
	}
	if q.IsZero() {
		return []string{invalid(path+"[storage]", q.String(), "must be greater than zero")}
	}
	return nil
}

func validateVolumeMode(path string, mode *string) []string {
	if mode != nil && *mode != corev1.PersistentVolumeFilesystem && *mode != corev1.PersistentVolumeBlock {
		return []string{notSupported(path, *mode, []string{corev1.PersistentVolumeBlock, corev1.PersistentVolumeFilesystem})}
	}
	return nil
}

func validateClassName(path, name string) []string {
	if name != "" && !validation.IsDNS1123Subdomain(name) {
		return []string{invalid(path, name, validation.DNS1123SubdomainMessage)}
	}
	return nil
}

// ValidatePersistentVolumeClaim checks a claim's spec.
func ValidatePersistentVolumeClaim(pvc *corev1.PersistentVolumeClaim) []string {
	s := pvc.Spec
	errs := validateAccessModes("spec.accessModes", s.AccessModes)
	if s.Selector != nil {
		if _, err := s.Selector.AsSelector(); err != nil {
			errs = append(errs, "spec.selector: Invalid value: "+err.Error())
		}
	}
	errs = append(errs, validateStorageAmount("spec.resources", s.Resources.Requests)...)
	if s.StorageClassName != nil {
		errs = append(errs, validateClassName("spec.storageClassName", *s.StorageClassName)...)
	}
	return append(errs, validateVolumeMode("spec.volumeMode", s.VolumeMode)...)
}

// ValidatePersistentVolumeClaimUpdate keeps the spec of a claim, except
// that the volume it binds to may be filled in once.
func ValidatePersistentVolumeClaimUpdate(pvc, old *corev1.PersistentVolumeClaim) []string {
	oldSpec := old.Spec
	if oldSpec.VolumeName == "" {
		oldSpec.VolumeName = pvc.Spec.VolumeName
	}
	if !reflect.DeepEqual(pvc.Spec, oldSpec) {
		return []string{"spec: Forbidden: spec is immutable after creation except resources.requests and volumeAttributesClassName for bound claims"}
	}
	return nil
}

// ValidatePersistentVolume checks a volume's spec.
func ValidatePersistentVolume(pv *corev1.PersistentVolume) []string {
	s := pv.Spec
	var errs []string
	if len(s.Capacity) == 0 {
		errs = append(errs, "spec.capacity: Required value")
	} else {
		errs = append(errs, validateStorageAmount("spec.capacity", s.Capacity)...)
	}
	errs = append(errs, validateAccessModes("spec.accessModes", s.AccessModes)...)
	switch p := s.PersistentVolumeReclaimPolicy; p {
	case "", corev1.PersistentVolumeReclaimRetain, corev1.PersistentVolumeReclaimDelete, corev1.PersistentVolumeReclaimRecycle:
	default:
		errs = append(errs, notSupported("spec.persistentVolumeReclaimPolicy", p,
			[]string{corev1.PersistentVolumeReclaimDelete, corev1.PersistentVolumeReclaimRecycle, corev1.PersistentVolumeReclaimRetain}))
	}
	errs = append(errs, validateClassName("spec.storageClassName", s.StorageClassName)...)
	errs = append(errs, validateVolumeMode("spec.volumeMode", s.VolumeMode)...)
	kinds := 0
	if h := s.HostPath; h != nil {
		kinds++
		errs = append(errs, validateHostPath("spec.hostPath", h)...)
	}
	if l := s.Local; l != nil {
		if kinds > 0 {
			errs = append(errs, "spec.local: Forbidden: may not specify more than 1 volume type")
		}
		kinds++
		if !strings.HasPrefix(l.Path, "/") {
			errs = append(errs, invalid("spec.local.path", l.Path, "must be an absolute path"))
		}
		if s.NodeAffinity == nil || s.NodeAffinity.Required == nil {
			errs = append(errs, "spec.nodeAffinity: Required value: Local volume requires node affinity")
		}
	}
	if kinds == 0 {
		errs = append(errs, "spec: Required value: must specify a volume type")
	}
	if a := s.NodeAffinity; a != nil {
		if a.Required == nil || len(a.Required.NodeSelectorTerms) == 0 {
			errs = append(errs, "spec.nodeAffinity.required.nodeSelectorTerms: Required value: must have at least one node selector term")
		} else {
			for i, t := range a.Required.NodeSelectorTerms {
				errs = append(errs, validateNodeSelectorTerm(fmt.Sprintf("spec.nodeAffinity.required.nodeSelectorTerms[%d]", i), t)...)
			}
		}
	}
	if ref := s.ClaimRef; ref != nil && (ref.Name == "" || ref.Namespace == "") {
		errs = append(errs, "spec.claimRef: Required value: name and namespace are required")
	}
	return errs
}

// ValidatePersistentVolumeUpdate keeps where the data lives.
func ValidatePersistentVolumeUpdate(pv, old *corev1.PersistentVolume) []string {
	var errs []string
	if !reflect.DeepEqual(pv.Spec.PersistentVolumeSource, old.Spec.PersistentVolumeSource) {
		errs = append(errs, "spec.persistentvolumesource: Forbidden: spec.persistentvolumesource is immutable after creation")
	}
	if !reflect.DeepEqual(pv.Spec.VolumeMode, old.Spec.VolumeMode) {
		errs = append(errs, invalid("spec.volumeMode", fmt.Sprint(deref(pv.Spec.VolumeMode)), "field is immutable"))
	}
	if old.Spec.NodeAffinity != nil && !reflect.DeepEqual(pv.Spec.NodeAffinity, old.Spec.NodeAffinity) {
		errs = append(errs, "spec.nodeAffinity: Invalid value: field is immutable")
	}
	return errs
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
