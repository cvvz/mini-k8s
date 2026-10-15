package plugins

import (
	"fmt"
	"sort"

	corev1 "mini-k8s/pkg/apis/core/v1"
	storagev1 "mini-k8s/pkg/apis/storage/v1"
	"mini-k8s/pkg/scheduler/framework"
)

// Reasons of the VolumeBinding plugin, as kube-scheduler words them.
const (
	ReasonUnboundImmediate   = "pod has unbound immediate PersistentVolumeClaims"
	ReasonVolumeNodeConflict = "node(s) had volume node affinity conflict"
	ReasonVolumeBindConflict = "node(s) didn't find available persistent volumes to bind"
	ReasonVolumeNotExist     = "node(s) unavailable due to one or more pvc(s) bound to non-existent pv(s)"
)

// noProvisioner marks classes of statically provisioned volumes only.
const noProvisioner = "kubernetes.io/no-provisioner"

// VolumeLister reads claims, volumes, classes and nodes (the scheduler's
// caches) and the claims the scheduler is binding right now.
type VolumeLister interface {
	Claim(namespace, name string) (*corev1.PersistentVolumeClaim, bool)
	Volume(name string) (*corev1.PersistentVolume, bool)
	Volumes() []*corev1.PersistentVolume
	Class(name string) (*storagev1.StorageClass, bool)
	NodeExists(name string) bool
	// Assumed returns the binding in progress for a claim, if any.
	Assumed(namespace, name string) (ClaimAssumption, bool)
}

// ClaimAssumption is where a claim is being bound: provisioned for Node,
// or reserved the static Volume.
type ClaimAssumption struct {
	Node, Volume string
}

// VolumeBinding places Pods with PersistentVolumeClaims, like
// kube-scheduler's plugin: the claims must exist, claims of Immediate
// classes must be bound already, bound volumes must be usable from the
// node (their node affinity), and claims that wait for their first
// consumer need a matching available volume on the node or a provisioner.
type VolumeBinding struct{ Lister VolumeLister }

func (VolumeBinding) Name() string { return "VolumeBinding" }

// ClaimBinding is how a claim gets its volume once the Pod's node is
// chosen: the static Volume reserved for it, or provisioning (Volume nil).
type ClaimBinding struct {
	Claim  *corev1.PersistentVolumeClaim
	Volume *corev1.PersistentVolume
}

// Claims returns the Pod's claims, or why they cannot be used.
func (v VolumeBinding) Claims(pod *corev1.Pod) ([]*corev1.PersistentVolumeClaim, string) {
	var claims []*corev1.PersistentVolumeClaim
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}
		name := vol.PersistentVolumeClaim.ClaimName
		pvc, ok := v.Lister.Claim(pod.Namespace, name)
		if !ok {
			return nil, fmt.Sprintf("persistentvolumeclaim %q not found", name)
		}
		if pvc.DeletionTimestamp != nil {
			return nil, fmt.Sprintf("persistentvolumeclaim %q is being deleted", name)
		}
		claims = append(claims, pvc)
	}
	return claims, ""
}

// delayed reports whether an unbound claim waits for its first consumer.
func (v VolumeBinding) delayed(pvc *corev1.PersistentVolumeClaim) bool {
	sc, ok := v.Lister.Class(corev1.ClaimClass(pvc))
	return ok && storagev1.WaitsForConsumer(sc)
}

func (v VolumeBinding) PreFilter(pod *corev1.Pod, _ *framework.Snapshot) string {
	claims, reason := v.Claims(pod)
	if reason != "" {
		return reason
	}
	for _, pvc := range claims {
		if pvc.Spec.VolumeName == "" && !v.delayed(pvc) {
			return ReasonUnboundImmediate
		}
	}
	return ""
}

func (v VolumeBinding) Filter(pod *corev1.Pod, node *framework.NodeInfo, _ *framework.Snapshot) []string {
	if _, reason := v.Bindings(pod, node.Node); reason != "" {
		return []string{reason}
	}
	return nil
}

// Bindings returns what the Pod's unbound claims need on the node, or the
// reason the node does not fit.
func (v VolumeBinding) Bindings(pod *corev1.Pod, node *corev1.Node) ([]ClaimBinding, string) {
	claims, reason := v.Claims(pod)
	if reason != "" {
		return nil, reason
	}
	var out []ClaimBinding
	taken := map[string]bool{}
	for _, pvc := range claims {
		if pvc.Spec.VolumeName != "" {
			pv, ok := v.Lister.Volume(pvc.Spec.VolumeName)
			if !ok {
				return nil, ReasonVolumeNotExist
			}
			if !corev1.VolumeFitsNode(pv, node) {
				return nil, ReasonVolumeNodeConflict
			}
			continue
		}
		// Another Pod sharing the claim is binding it: follow it there.
		if a, ok := v.Lister.Assumed(pvc.Namespace, pvc.Name); ok {
			if a.Volume != "" {
				pv, ok := v.Lister.Volume(a.Volume)
				if !ok || !corev1.VolumeFitsNode(pv, node) {
					return nil, ReasonVolumeNodeConflict
				}
				out = append(out, ClaimBinding{Claim: pvc, Volume: pv})
			} else if a.Node != node.Name {
				return nil, ReasonVolumeNodeConflict
			} else {
				out = append(out, ClaimBinding{Claim: pvc})
			}
			continue
		}
		// A node chosen earlier stays chosen while it exists.
		if selected := pvc.Annotations[corev1.AnnSelectedNode]; selected != "" && selected != node.Name && v.Lister.NodeExists(selected) {
			return nil, ReasonVolumeBindConflict
		}
		if pv := v.availableVolume(pvc, node, taken); pv != nil {
			taken[pv.Name] = true
			out = append(out, ClaimBinding{Claim: pvc, Volume: pv})
			continue
		}
		if sc, ok := v.Lister.Class(corev1.ClaimClass(pvc)); ok && sc.Provisioner != noProvisioner {
			out = append(out, ClaimBinding{Claim: pvc})
			continue
		}
		return nil, ReasonVolumeBindConflict
	}
	return out, ""
}

// availableVolume picks the smallest available volume (or one reserved
// for the claim) that satisfies the claim and is usable from the node.
func (v VolumeBinding) availableVolume(pvc *corev1.PersistentVolumeClaim, node *corev1.Node, taken map[string]bool) *corev1.PersistentVolume {
	var fits []*corev1.PersistentVolume
	for _, pv := range v.Lister.Volumes() {
		if taken[pv.Name] || pv.DeletionTimestamp != nil || !corev1.VolumeSatisfiesClaim(pv, pvc) || !corev1.VolumeFitsNode(pv, node) {
			continue
		}
		if ref := pv.Spec.ClaimRef; ref != nil {
			if ref.Namespace == pvc.Namespace && ref.Name == pvc.Name && (ref.UID == "" || ref.UID == pvc.UID) {
				return pv
			}
			continue
		}
		if pv.Status.Phase == corev1.VolumeAvailable {
			fits = append(fits, pv)
		}
	}
	sort.Slice(fits, func(i, j int) bool {
		a, b := fits[i].Spec.Capacity[corev1.ResourceStorage], fits[j].Spec.Capacity[corev1.ResourceStorage]
		if c := a.Cmp(b); c != 0 {
			return c < 0
		}
		return fits[i].Name < fits[j].Name
	})
	if len(fits) == 0 {
		return nil
	}
	return fits[0]
}
