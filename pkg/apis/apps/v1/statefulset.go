package v1

import (
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/ptr"
)

// StatefulSet runs Pods with stable identities: names <set>-<ordinal>, a
// host name under the governing Service and a claim per volume template.
type StatefulSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              StatefulSetSpec   `json:"spec"`
	Status            StatefulSetStatus `json:"status,omitzero"`
}

// StatefulSetSpec is the desired state.
type StatefulSetSpec struct {
	Replicas *int32                 `json:"replicas,omitempty"`
	Selector *metav1.LabelSelector  `json:"selector"`
	Template corev1.PodTemplateSpec `json:"template"`
	// VolumeClaimTemplates become one claim per Pod, <template>-<pod>.
	VolumeClaimTemplates []corev1.PersistentVolumeClaim `json:"volumeClaimTemplates,omitempty"`
	// ServiceName is the headless Service that gives Pods their DNS names.
	ServiceName string `json:"serviceName,omitempty"`
	// PodManagementPolicy is OrderedReady (one at a time, in order) or
	// Parallel.
	PodManagementPolicy  string                    `json:"podManagementPolicy,omitempty"`
	UpdateStrategy       StatefulSetUpdateStrategy `json:"updateStrategy,omitzero"`
	RevisionHistoryLimit *int32                    `json:"revisionHistoryLimit,omitempty"`
	MinReadySeconds      int32                     `json:"minReadySeconds,omitempty"`
	// PersistentVolumeClaimRetentionPolicy decides whether claims go with
	// the set or with scaled-down Pods.
	PersistentVolumeClaimRetentionPolicy *StatefulSetPersistentVolumeClaimRetentionPolicy `json:"persistentVolumeClaimRetentionPolicy,omitempty"`
	// Ordinals numbers the Pods from Start (0 by default).
	Ordinals *StatefulSetOrdinals `json:"ordinals,omitempty"`
}

// Pod management policies.
const (
	OrderedReadyPodManagement = "OrderedReady"
	ParallelPodManagement     = "Parallel"
)

// StatefulSet update strategies.
const (
	RollingUpdateStatefulSetStrategyType = "RollingUpdate"
	OnDeleteStatefulSetStrategyType      = "OnDelete"
)

// StatefulSetUpdateStrategy describes how Pods are replaced.
type StatefulSetUpdateStrategy struct {
	Type          string                            `json:"type,omitempty"`
	RollingUpdate *RollingUpdateStatefulSetStrategy `json:"rollingUpdate,omitempty"`
}

// RollingUpdateStatefulSetStrategy keeps Pods with an ordinal below
// Partition at the current revision.
type RollingUpdateStatefulSetStrategy struct {
	Partition *int32 `json:"partition,omitempty"`
}

// Claim retention policies.
const (
	RetainPersistentVolumeClaimRetentionPolicyType = "Retain"
	DeletePersistentVolumeClaimRetentionPolicyType = "Delete"
)

// StatefulSetPersistentVolumeClaimRetentionPolicy says what happens to
// claims when the set is deleted or scaled down.
type StatefulSetPersistentVolumeClaimRetentionPolicy struct {
	WhenDeleted string `json:"whenDeleted,omitempty"`
	WhenScaled  string `json:"whenScaled,omitempty"`
}

// StatefulSetOrdinals sets the first ordinal.
type StatefulSetOrdinals struct {
	Start int32 `json:"start,omitempty"`
}

// StatefulSetStatus is maintained by the StatefulSet controller.
type StatefulSetStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	Replicas           int32 `json:"replicas"`
	ReadyReplicas      int32 `json:"readyReplicas,omitempty"`
	// CurrentReplicas run CurrentRevision, UpdatedReplicas UpdateRevision.
	CurrentReplicas   int32  `json:"currentReplicas,omitempty"`
	UpdatedReplicas   int32  `json:"updatedReplicas,omitempty"`
	CurrentRevision   string `json:"currentRevision,omitempty"`
	UpdateRevision    string `json:"updateRevision,omitempty"`
	CollisionCount    *int32 `json:"collisionCount,omitempty"`
	AvailableReplicas int32  `json:"availableReplicas"`
}

// Labels of StatefulSet Pods and revisions.
const (
	// StatefulSetPodNameLabel selects a single Pod of a set (for a
	// per-Pod Service, say).
	StatefulSetPodNameLabel = "statefulset.kubernetes.io/pod-name"
	// PodIndexLabel carries the ordinal.
	PodIndexLabel = "apps.kubernetes.io/pod-index"
	// ControllerRevisionHashLabel holds the hash on StatefulSet revisions.
	ControllerRevisionHashLabel = "controller.kubernetes.io/hash"
)

// SetDefaultsStatefulSet fills omitted fields with Kubernetes' defaults.
func SetDefaultsStatefulSet(s *StatefulSet) {
	if s.Spec.Replicas == nil {
		s.Spec.Replicas = ptr.To[int32](1)
	}
	if s.Spec.PodManagementPolicy == "" {
		s.Spec.PodManagementPolicy = OrderedReadyPodManagement
	}
	if s.Spec.UpdateStrategy.Type == "" {
		s.Spec.UpdateStrategy.Type = RollingUpdateStatefulSetStrategyType
	}
	if s.Spec.UpdateStrategy.Type == RollingUpdateStatefulSetStrategyType {
		if s.Spec.UpdateStrategy.RollingUpdate == nil {
			s.Spec.UpdateStrategy.RollingUpdate = &RollingUpdateStatefulSetStrategy{}
		}
		if s.Spec.UpdateStrategy.RollingUpdate.Partition == nil {
			s.Spec.UpdateStrategy.RollingUpdate.Partition = ptr.To[int32](0)
		}
	}
	if s.Spec.RevisionHistoryLimit == nil {
		s.Spec.RevisionHistoryLimit = ptr.To[int32](10)
	}
	if s.Spec.PersistentVolumeClaimRetentionPolicy == nil {
		s.Spec.PersistentVolumeClaimRetentionPolicy = &StatefulSetPersistentVolumeClaimRetentionPolicy{}
	}
	if p := s.Spec.PersistentVolumeClaimRetentionPolicy; p.WhenDeleted == "" {
		p.WhenDeleted = RetainPersistentVolumeClaimRetentionPolicyType
	}
	if p := s.Spec.PersistentVolumeClaimRetentionPolicy; p.WhenScaled == "" {
		p.WhenScaled = RetainPersistentVolumeClaimRetentionPolicyType
	}
	for i := range s.Spec.VolumeClaimTemplates {
		if c := &s.Spec.VolumeClaimTemplates[i]; c.Spec.VolumeMode == nil {
			c.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeFilesystem)
		}
	}
	corev1.SetDefaultsPodSpec(&s.Spec.Template.Spec)
}

// StartOrdinal is the ordinal of the first Pod.
func (s *StatefulSet) StartOrdinal() int {
	if s.Spec.Ordinals != nil {
		return int(s.Spec.Ordinals.Start)
	}
	return 0
}
