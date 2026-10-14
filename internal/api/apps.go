package api

// Deployment maintains a set of identical Pods built from a template.
type Deployment struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Spec       DeploymentSpec   `json:"spec"`
	Status     DeploymentStatus `json:"status,omitzero"`
}

// DeploymentSpec is the desired state.
type DeploymentSpec struct {
	Replicas *int32          `json:"replicas,omitempty"`
	Selector *LabelSelector  `json:"selector"`
	Template PodTemplateSpec `json:"template"`
}

// PodTemplateSpec describes Pods created by a controller.
type PodTemplateSpec struct {
	ObjectMeta `json:"metadata"`
	Spec       PodSpec `json:"spec"`
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
