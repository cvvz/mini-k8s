package v1

import (
	"mini-k8s/pkg/api/resource"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

// PersistentVolume is a piece of storage in the cluster, provisioned by an
// administrator or dynamically for a claim.
type PersistentVolume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              PersistentVolumeSpec   `json:"spec"`
	Status            PersistentVolumeStatus `json:"status,omitzero"`
}

// PersistentVolumeSpec describes the storage and who may claim it.
type PersistentVolumeSpec struct {
	Capacity    ResourceList `json:"capacity,omitempty"`
	AccessModes []string     `json:"accessModes,omitempty"`
	// ClaimRef is the claim the volume is bound (or reserved) to.
	ClaimRef *ObjectReference `json:"claimRef,omitempty"`
	// PersistentVolumeReclaimPolicy is Retain (default for static volumes),
	// Delete or Recycle.
	PersistentVolumeReclaimPolicy string              `json:"persistentVolumeReclaimPolicy,omitempty"`
	StorageClassName              string              `json:"storageClassName,omitempty"`
	VolumeMode                    *string             `json:"volumeMode,omitempty"`
	NodeAffinity                  *VolumeNodeAffinity `json:"nodeAffinity,omitempty"`
	MountOptions                  []string            `json:"mountOptions,omitempty"`
	PersistentVolumeSource        `json:",inline"`
}

// PersistentVolumeSource is where the data lives: a directory of a node.
type PersistentVolumeSource struct {
	HostPath *HostPathVolumeSource `json:"hostPath,omitempty"`
	Local    *LocalVolumeSource    `json:"local,omitempty"`
}

// LocalVolumeSource is an existing directory of a node (with nodeAffinity).
type LocalVolumeSource struct {
	Path string `json:"path"`
}

// VolumeNodeAffinity restricts the nodes a volume can be used from.
type VolumeNodeAffinity struct {
	Required *NodeSelector `json:"required,omitempty"`
}

// PersistentVolumeStatus is the volume's lifecycle phase.
type PersistentVolumeStatus struct {
	Phase                   string       `json:"phase,omitempty"`
	Message                 string       `json:"message,omitempty"`
	Reason                  string       `json:"reason,omitempty"`
	LastPhaseTransitionTime *metav1.Time `json:"lastPhaseTransitionTime,omitempty"`
}

// PersistentVolumeClaim requests storage for Pods of its namespace.
type PersistentVolumeClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              PersistentVolumeClaimSpec   `json:"spec"`
	Status            PersistentVolumeClaimStatus `json:"status,omitzero"`
}

// PersistentVolumeClaimSpec is what the claim needs.
type PersistentVolumeClaimSpec struct {
	AccessModes []string              `json:"accessModes,omitempty"`
	Selector    *metav1.LabelSelector `json:"selector,omitempty"`
	Resources   VolumeResources       `json:"resources,omitzero"`
	// VolumeName binds the claim to that volume.
	VolumeName string `json:"volumeName,omitempty"`
	// StorageClassName nil means the default class; "" means none.
	StorageClassName *string `json:"storageClassName,omitempty"`
	VolumeMode       *string `json:"volumeMode,omitempty"`
}

// VolumeResources holds the requested storage.
type VolumeResources struct {
	Limits   ResourceList `json:"limits,omitempty"`
	Requests ResourceList `json:"requests,omitempty"`
}

// PersistentVolumeClaimStatus reports the binding.
type PersistentVolumeClaimStatus struct {
	Phase       string       `json:"phase,omitempty"`
	AccessModes []string     `json:"accessModes,omitempty"`
	Capacity    ResourceList `json:"capacity,omitempty"`
}

// PersistentVolumeClaimVolumeSource mounts the volume bound to a claim.
type PersistentVolumeClaimVolumeSource struct {
	ClaimName string `json:"claimName"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// Access modes, volume modes, reclaim policies and phases.
const (
	ReadWriteOnce    = "ReadWriteOnce"
	ReadOnlyMany     = "ReadOnlyMany"
	ReadWriteMany    = "ReadWriteMany"
	ReadWriteOncePod = "ReadWriteOncePod"

	PersistentVolumeFilesystem = "Filesystem"
	PersistentVolumeBlock      = "Block"

	PersistentVolumeReclaimRetain  = "Retain"
	PersistentVolumeReclaimDelete  = "Delete"
	PersistentVolumeReclaimRecycle = "Recycle"

	VolumePending   = "Pending"
	VolumeAvailable = "Available"
	VolumeBound     = "Bound"
	VolumeReleased  = "Released"
	VolumeFailed    = "Failed"

	ClaimPending = "Pending"
	ClaimBound   = "Bound"
	ClaimLost    = "Lost"

	ResourceStorage = "storage"
)

// Annotations and finalizers of the volume lifecycle.
const (
	AnnBindCompleted          = "pv.kubernetes.io/bind-completed"
	AnnBoundByController      = "pv.kubernetes.io/bound-by-controller"
	AnnSelectedNode           = "volume.kubernetes.io/selected-node"
	AnnStorageProvisioner     = "volume.kubernetes.io/storage-provisioner"
	AnnBetaStorageProvisioner = "volume.beta.kubernetes.io/storage-provisioner"
	AnnDynamicallyProvisioned = "pv.kubernetes.io/provisioned-by"
	PVCProtectionFinalizer    = "kubernetes.io/pvc-protection"
	PVProtectionFinalizer     = "kubernetes.io/pv-protection"
)

// ClaimClass returns the claim's class ("" for none).
func ClaimClass(pvc *PersistentVolumeClaim) string {
	if pvc.Spec.StorageClassName == nil {
		return ""
	}
	return *pvc.Spec.StorageClassName
}

// ClaimRequest returns the requested storage.
func ClaimRequest(pvc *PersistentVolumeClaim) resource.Quantity {
	return pvc.Spec.Resources.Requests[ResourceStorage]
}

// Matches reports whether the node satisfies any of the terms.
func (s *NodeSelector) Matches(node *Node) bool {
	for _, term := range s.NodeSelectorTerms {
		if term.Matches(node) {
			return true
		}
	}
	return false
}

// VolumeFitsNode reports whether a volume may be used from the node.
func VolumeFitsNode(pv *PersistentVolume, node *Node) bool {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return true
	}
	return pv.Spec.NodeAffinity.Required.Matches(node)
}

// AccessModesShort abbreviates access modes as kubectl does: RWO,ROX,RWX,RWOP.
func AccessModesShort(modes []string) string {
	short := map[string]string{ReadWriteOnce: "RWO", ReadOnlyMany: "ROX", ReadWriteMany: "RWX", ReadWriteOncePod: "RWOP"}
	out := ""
	for _, m := range []string{ReadWriteOncePod, ReadWriteOnce, ReadOnlyMany, ReadWriteMany} {
		for _, have := range modes {
			if have == m {
				if out != "" {
					out += ","
				}
				out += short[m]
			}
		}
	}
	return out
}

// VolumeSatisfiesClaim reports whether a volume can serve a claim: the same class,
// every requested access mode, enough capacity, the same volume mode and
// labels matching the claim's selector.
func VolumeSatisfiesClaim(pv *PersistentVolume, pvc *PersistentVolumeClaim) bool {
	if pv.Spec.StorageClassName != ClaimClass(pvc) {
		return false
	}
	for _, want := range pvc.Spec.AccessModes {
		found := false
		for _, have := range pv.Spec.AccessModes {
			found = found || want == have
		}
		if !found {
			return false
		}
	}
	request := ClaimRequest(pvc)
	if capacity := pv.Spec.Capacity[ResourceStorage]; capacity.Cmp(request) < 0 {
		return false
	}
	if volumeModeOf(pv.Spec.VolumeMode) != volumeModeOf(pvc.Spec.VolumeMode) {
		return false
	}
	if pvc.Spec.Selector != nil {
		sel, err := pvc.Spec.Selector.AsSelector()
		if err != nil || !sel.Matches(pv.Labels) {
			return false
		}
	}
	return true
}

func volumeModeOf(m *string) string {
	if m == nil {
		return PersistentVolumeFilesystem
	}
	return *m
}
