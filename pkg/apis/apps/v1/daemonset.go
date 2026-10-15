package v1

import (
	"encoding/json"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/intstr"
)

// DaemonSet runs a Pod on every node that matches its template.
type DaemonSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              DaemonSetSpec   `json:"spec"`
	Status            DaemonSetStatus `json:"status,omitzero"`
}

// DaemonSetSpec is the desired state.
type DaemonSetSpec struct {
	Selector *metav1.LabelSelector  `json:"selector"`
	Template corev1.PodTemplateSpec `json:"template"`
	// UpdateStrategy replaces Pods after a template change (RollingUpdate
	// by default) or only once they are deleted (OnDelete).
	UpdateStrategy DaemonSetUpdateStrategy `json:"updateStrategy,omitzero"`
	// MinReadySeconds a new Pod must be ready to count as available.
	MinReadySeconds int32 `json:"minReadySeconds,omitempty"`
	// RevisionHistoryLimit is how many old ControllerRevisions are kept.
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`
}

// DaemonSet update strategies.
const (
	RollingUpdateDaemonSetStrategyType = "RollingUpdate"
	OnDeleteDaemonSetStrategyType      = "OnDelete"
)

// DaemonSetUpdateStrategy describes how Pods are replaced.
type DaemonSetUpdateStrategy struct {
	Type          string                  `json:"type,omitempty"`
	RollingUpdate *RollingUpdateDaemonSet `json:"rollingUpdate,omitempty"`
}

// RollingUpdateDaemonSet bounds a rolling update, as numbers or percentages
// of the nodes that should run the Pod (rounded up).
type RollingUpdateDaemonSet struct {
	// MaxUnavailable nodes without an available Pod (1 by default).
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
	// MaxSurge nodes running an old and a new Pod at once (0 by default).
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`
}

// DaemonSetStatus is maintained by the DaemonSet controller.
type DaemonSetStatus struct {
	CurrentNumberScheduled int32  `json:"currentNumberScheduled"`
	NumberMisscheduled     int32  `json:"numberMisscheduled"`
	DesiredNumberScheduled int32  `json:"desiredNumberScheduled"`
	NumberReady            int32  `json:"numberReady"`
	ObservedGeneration     int64  `json:"observedGeneration,omitempty"`
	UpdatedNumberScheduled int32  `json:"updatedNumberScheduled,omitempty"`
	NumberAvailable        int32  `json:"numberAvailable,omitempty"`
	NumberUnavailable      int32  `json:"numberUnavailable,omitempty"`
	CollisionCount         *int32 `json:"collisionCount,omitempty"`
}

// Labels and annotations of DaemonSet Pods and revisions.
const (
	// ControllerRevisionHashLabelKey names the revision a Pod was made from.
	ControllerRevisionHashLabelKey = "controller-revision-hash"
	// DaemonSetTemplateGenerationKey carries the template generation on Pods.
	DaemonSetTemplateGenerationKey = "pod-template-generation"
	// DeprecatedTemplateGeneration counts the template changes of a DaemonSet.
	DeprecatedTemplateGeneration = "deprecated.daemonset.template.generation"
)

// ControllerRevision is an immutable snapshot of a controller's template,
// numbered by revision, for rollouts and rollbacks.
type ControllerRevision struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	// Data is the template as a patch of the controller's spec, like
	// {"spec":{"template":{..., "$patch":"replace"}}}.
	Data     json.RawMessage `json:"data,omitempty"`
	Revision int64           `json:"revision"`
}
