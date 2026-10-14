package api

// Namespace groups namespaced objects; deleting it deletes its contents.
type Namespace struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Spec       NamespaceSpec   `json:"spec,omitzero"`
	Status     NamespaceStatus `json:"status,omitzero"`
}

// NamespaceSpec is empty; namespace finalization uses metadata.finalizers.
type NamespaceSpec struct{}

// NamespaceStatus reports the lifecycle phase.
type NamespaceStatus struct {
	Phase string `json:"phase,omitempty"`
}

// Namespace phases.
const (
	NamespaceActive      = "Active"
	NamespaceTerminating = "Terminating"
)

// Well-known namespaces created by the API server.
const (
	NamespaceDefault = "default"
	NamespaceSystem  = "kube-system"
)

// Node is a worker registered by its kubelet.
type Node struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Spec       NodeSpec   `json:"spec,omitzero"`
	Status     NodeStatus `json:"status,omitzero"`
}

// NodeSpec holds scheduling controls.
type NodeSpec struct {
	Unschedulable bool `json:"unschedulable,omitempty"`
}

// NodeStatus is reported by the kubelet.
type NodeStatus struct {
	Conditions []NodeCondition `json:"conditions,omitempty"`
	Addresses  []NodeAddress   `json:"addresses,omitempty"`
	NodeInfo   NodeSystemInfo  `json:"nodeInfo,omitzero"`
}

// NodeCondition is one aspect of node health.
type NodeCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	LastHeartbeatTime  Time   `json:"lastHeartbeatTime,omitzero"`
	LastTransitionTime Time   `json:"lastTransitionTime,omitzero"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
}

// NodeAddress is a reachable address of the node.
type NodeAddress struct {
	Type    string `json:"type"`
	Address string `json:"address"`
}

// NodeSystemInfo describes the node software.
type NodeSystemInfo struct {
	KubeletVersion          string `json:"kubeletVersion,omitempty"`
	ContainerRuntimeVersion string `json:"containerRuntimeVersion,omitempty"`
	OperatingSystem         string `json:"operatingSystem,omitempty"`
	Architecture            string `json:"architecture,omitempty"`
}

// Condition types and statuses.
const (
	NodeReady         = "Ready"
	NodeInternalIP    = "InternalIP"
	NodeHostName      = "Hostname"
	ConditionTrue     = "True"
	ConditionFalse    = "False"
	ConditionUnknown  = "Unknown"
	LabelHostname     = "kubernetes.io/hostname"
	LabelOS           = "kubernetes.io/os"
	LabelArch         = "kubernetes.io/arch"
	LabelTemplateHash = "pod-template-hash"
)

// NodeCondition returns the condition of the given type, if present.
func (s *NodeStatus) Condition(t string) *NodeCondition {
	for i := range s.Conditions {
		if s.Conditions[i].Type == t {
			return &s.Conditions[i]
		}
	}
	return nil
}

// InternalIP returns the node's InternalIP address, if reported.
func (n *Node) InternalIP() string {
	for _, a := range n.Status.Addresses {
		if a.Type == NodeInternalIP {
			return a.Address
		}
	}
	return ""
}

// Pod is a group of containers scheduled onto one node.
type Pod struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Spec       PodSpec   `json:"spec"`
	Status     PodStatus `json:"status,omitzero"`
}

// PodSpec is the desired state of a Pod.
type PodSpec struct {
	Containers                    []Container `json:"containers"`
	TerminationGracePeriodSeconds *int64      `json:"terminationGracePeriodSeconds,omitempty"`
	NodeName                      string      `json:"nodeName,omitempty"`
}

// Container is one process image in a Pod.
type Container struct {
	Name    string          `json:"name"`
	Image   string          `json:"image"`
	Command []string        `json:"command,omitempty"`
	Args    []string        `json:"args,omitempty"`
	Env     []EnvVar        `json:"env,omitempty"`
	Ports   []ContainerPort `json:"ports,omitempty"`
}

// EnvVar is a literal environment variable.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ContainerPort documents a port the container listens on.
type ContainerPort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int32  `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}

// PodStatus is reported by the scheduler and kubelet.
type PodStatus struct {
	Phase             string            `json:"phase,omitempty"`
	Conditions        []PodCondition    `json:"conditions,omitempty"`
	Message           string            `json:"message,omitempty"`
	Reason            string            `json:"reason,omitempty"`
	HostIP            string            `json:"hostIP,omitempty"`
	PodIP             string            `json:"podIP,omitempty"`
	StartTime         *Time             `json:"startTime,omitempty"`
	ContainerStatuses []ContainerStatus `json:"containerStatuses,omitempty"`
}

// Pod phases.
const (
	PodPending   = "Pending"
	PodRunning   = "Running"
	PodSucceeded = "Succeeded"
	PodFailed    = "Failed"
)

// Pod condition types.
const (
	PodScheduled    = "PodScheduled"
	PodInitialized  = "Initialized"
	ContainersReady = "ContainersReady"
	PodReady        = "Ready"
)

// PodCondition is one aspect of Pod state.
type PodCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	LastTransitionTime Time   `json:"lastTransitionTime,omitzero"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
}

// ContainerStatus reports one container.
type ContainerStatus struct {
	Name                 string         `json:"name"`
	State                ContainerState `json:"state,omitzero"`
	LastTerminationState ContainerState `json:"lastState,omitzero"`
	Ready                bool           `json:"ready"`
	RestartCount         int32          `json:"restartCount"`
	Image                string         `json:"image"`
	ImageID              string         `json:"imageID,omitempty"`
	ContainerID          string         `json:"containerID,omitempty"`
}

// ContainerState holds exactly one of its members.
type ContainerState struct {
	Waiting    *ContainerStateWaiting    `json:"waiting,omitempty"`
	Running    *ContainerStateRunning    `json:"running,omitempty"`
	Terminated *ContainerStateTerminated `json:"terminated,omitempty"`
}

// ContainerStateWaiting explains why a container is not running yet.
type ContainerStateWaiting struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ContainerStateRunning records when a container started.
type ContainerStateRunning struct {
	StartedAt Time `json:"startedAt,omitzero"`
}

// ContainerStateTerminated records how a container exited.
type ContainerStateTerminated struct {
	ExitCode    int32  `json:"exitCode"`
	Reason      string `json:"reason,omitempty"`
	Message     string `json:"message,omitempty"`
	StartedAt   Time   `json:"startedAt,omitzero"`
	FinishedAt  Time   `json:"finishedAt,omitzero"`
	ContainerID string `json:"containerID,omitempty"`
}

// Condition returns the Pod condition of the given type, if present.
func (s *PodStatus) Condition(t string) *PodCondition {
	for i := range s.Conditions {
		if s.Conditions[i].Type == t {
			return &s.Conditions[i]
		}
	}
	return nil
}

// SetCondition inserts or updates a condition, keeping LastTransitionTime
// when the status does not change. It reports whether anything changed.
func (s *PodStatus) SetCondition(c PodCondition) bool {
	if existing := s.Condition(c.Type); existing != nil {
		if existing.Status == c.Status && existing.Reason == c.Reason && existing.Message == c.Message {
			return false
		}
		if existing.Status == c.Status {
			c.LastTransitionTime = existing.LastTransitionTime
		}
		*existing = c
		return true
	}
	s.Conditions = append(s.Conditions, c)
	return true
}

// IsPodReady reports whether the Pod's Ready condition is True.
func IsPodReady(p *Pod) bool {
	c := p.Status.Condition(PodReady)
	return c != nil && c.Status == ConditionTrue
}

// Service exposes Pods selected by labels.
type Service struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Spec       ServiceSpec   `json:"spec"`
	Status     ServiceStatus `json:"status,omitzero"`
}

// ServiceSpec selects backends and ports.
type ServiceSpec struct {
	Selector map[string]string `json:"selector,omitempty"`
	Ports    []ServicePort     `json:"ports"`
}

// ServicePort maps a Service port to a container port.
type ServicePort struct {
	Name       string `json:"name,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	Port       int32  `json:"port"`
	TargetPort int32  `json:"targetPort,omitempty"`
}

// ServiceStatus is currently empty.
type ServiceStatus struct{}

// Event records something that happened to an object.
type Event struct {
	TypeMeta       `json:",inline"`
	ObjectMeta     `json:"metadata"`
	InvolvedObject ObjectReference `json:"involvedObject"`
	Reason         string          `json:"reason,omitempty"`
	Message        string          `json:"message,omitempty"`
	Source         EventSource     `json:"source,omitzero"`
	FirstTimestamp Time            `json:"firstTimestamp,omitzero"`
	LastTimestamp  Time            `json:"lastTimestamp,omitzero"`
	Count          int32           `json:"count,omitempty"`
	Type           string          `json:"type,omitempty"`
}

// Event types.
const (
	EventTypeNormal  = "Normal"
	EventTypeWarning = "Warning"
)

// ObjectReference points at another object.
type ObjectReference struct {
	Kind       string `json:"kind,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name,omitempty"`
	UID        string `json:"uid,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
}

// EventSource names the reporting component.
type EventSource struct {
	Component string `json:"component,omitempty"`
	Host      string `json:"host,omitempty"`
}

// Binding assigns a Pod to a node (POST pods/{name}/binding).
type Binding struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Target     ObjectReference `json:"target"`
}
