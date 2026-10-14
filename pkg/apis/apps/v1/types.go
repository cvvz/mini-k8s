// Package v1 contains the apps API group types (appsv1).
package v1

import (
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/intstr"
)

// DefaultDeploymentUniqueLabelKey labels the ReplicaSets of a Deployment and
// their Pods with the hash of the template they were built from.
const DefaultDeploymentUniqueLabelKey = "pod-template-hash"

// Annotations maintained by the Deployment controller and kk.
const (
	// RevisionAnnotation numbers a Deployment's ReplicaSets; the Deployment
	// carries the revision of its newest one.
	RevisionAnnotation = "deployment.kubernetes.io/revision"
	// ChangeCauseAnnotation explains a revision in `kk rollout history`.
	ChangeCauseAnnotation = "kubernetes.io/change-cause"
	// RestartedAtAnnotation is set on the Pod template by `kk rollout restart`.
	RestartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"
)

// Deployment maintains a set of identical Pods built from a template,
// through one ReplicaSet per template version.
type Deployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              DeploymentSpec   `json:"spec"`
	Status            DeploymentStatus `json:"status,omitzero"`
}

// DeploymentSpec is the desired state.
type DeploymentSpec struct {
	Replicas *int32                 `json:"replicas,omitempty"`
	Selector *metav1.LabelSelector  `json:"selector"`
	Template corev1.PodTemplateSpec `json:"template"`
	// Strategy replaces old Pods with new ones (RollingUpdate by default).
	Strategy DeploymentStrategy `json:"strategy,omitzero"`
	// MinReadySeconds a new Pod must be ready to count as available.
	MinReadySeconds int32 `json:"minReadySeconds,omitempty"`
	// RevisionHistoryLimit is how many old ReplicaSets are kept for rollbacks.
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`
	// Paused stops rollouts of template changes until resumed.
	Paused bool `json:"paused,omitempty"`
	// ProgressDeadlineSeconds after which a stalled rollout is reported.
	ProgressDeadlineSeconds *int32 `json:"progressDeadlineSeconds,omitempty"`
}

// Deployment strategy types.
const (
	RecreateDeploymentStrategyType      = "Recreate"
	RollingUpdateDeploymentStrategyType = "RollingUpdate"
)

// DeploymentStrategy describes how Pods are replaced.
type DeploymentStrategy struct {
	Type          string                   `json:"type,omitempty"`
	RollingUpdate *RollingUpdateDeployment `json:"rollingUpdate,omitempty"`
}

// RollingUpdateDeployment bounds a rolling update: numbers or percentages
// of the desired replicas.
type RollingUpdateDeployment struct {
	// MaxUnavailable Pods during the update (rounded down; 25% by default).
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
	// MaxSurge Pods above the desired count (rounded up; 25% by default).
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`
}

// DeploymentStatus is maintained by the Deployment controller.
type DeploymentStatus struct {
	ObservedGeneration  int64                 `json:"observedGeneration,omitempty"`
	Replicas            int32                 `json:"replicas,omitempty"`
	UpdatedReplicas     int32                 `json:"updatedReplicas,omitempty"`
	ReadyReplicas       int32                 `json:"readyReplicas,omitempty"`
	AvailableReplicas   int32                 `json:"availableReplicas,omitempty"`
	UnavailableReplicas int32                 `json:"unavailableReplicas,omitempty"`
	Conditions          []DeploymentCondition `json:"conditions,omitempty"`
}

// Deployment condition types.
const (
	DeploymentAvailable   = "Available"
	DeploymentProgressing = "Progressing"
)

// DeploymentCondition is one aspect of a Deployment's state.
type DeploymentCondition struct {
	Type               string      `json:"type"`
	Status             string      `json:"status"`
	LastUpdateTime     metav1.Time `json:"lastUpdateTime,omitzero"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitzero"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
}

// Condition returns the condition of the given type, if present.
func (s *DeploymentStatus) Condition(t string) *DeploymentCondition {
	for i := range s.Conditions {
		if s.Conditions[i].Type == t {
			return &s.Conditions[i]
		}
	}
	return nil
}

// ReplicaSet keeps a number of identical Pods running.
type ReplicaSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              ReplicaSetSpec   `json:"spec"`
	Status            ReplicaSetStatus `json:"status,omitzero"`
}

// ReplicaSetSpec is the desired state.
type ReplicaSetSpec struct {
	Replicas *int32 `json:"replicas,omitempty"`
	// MinReadySeconds a new Pod must be ready to count as available.
	MinReadySeconds int32                  `json:"minReadySeconds,omitempty"`
	Selector        *metav1.LabelSelector  `json:"selector"`
	Template        corev1.PodTemplateSpec `json:"template"`
}

// ReplicaSetStatus is maintained by the ReplicaSet controller.
type ReplicaSetStatus struct {
	Replicas             int32 `json:"replicas"`
	FullyLabeledReplicas int32 `json:"fullyLabeledReplicas,omitempty"`
	ReadyReplicas        int32 `json:"readyReplicas,omitempty"`
	AvailableReplicas    int32 `json:"availableReplicas,omitempty"`
	ObservedGeneration   int64 `json:"observedGeneration,omitempty"`
}
