// Package v1 contains the apps API group types (appsv1).
package v1

import (
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

// DefaultDeploymentUniqueLabelKey labels Pods with the hash of the template
// they were built from.
const DefaultDeploymentUniqueLabelKey = "pod-template-hash"

// Deployment maintains a set of identical Pods built from a template.
type Deployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec       DeploymentSpec   `json:"spec"`
	Status     DeploymentStatus `json:"status,omitzero"`
}

// DeploymentSpec is the desired state.
type DeploymentSpec struct {
	Replicas *int32          `json:"replicas,omitempty"`
	Selector *metav1.LabelSelector `json:"selector"`
	Template corev1.PodTemplateSpec `json:"template"`
}

// DeploymentStatus is maintained by the Deployment controller.
type DeploymentStatus struct {
	ObservedGeneration  int64 `json:"observedGeneration,omitempty"`
	Replicas            int32 `json:"replicas,omitempty"`
	UpdatedReplicas     int32 `json:"updatedReplicas,omitempty"`
	ReadyReplicas       int32 `json:"readyReplicas,omitempty"`
	AvailableReplicas   int32 `json:"availableReplicas,omitempty"`
	UnavailableReplicas int32 `json:"unavailableReplicas,omitempty"`
}
