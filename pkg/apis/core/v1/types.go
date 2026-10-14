// Package v1 contains the core API group types (corev1): Namespace, Node,
// Pod, Service, Event and Binding.
package v1

import (
	"time"

	"mini-k8s/pkg/api/resource"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/intstr"
)

// Namespace groups namespaced objects; deleting it deletes its contents.
type Namespace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              NamespaceSpec   `json:"spec,omitzero"`
	Status            NamespaceStatus `json:"status,omitzero"`
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

// FinalizerKubernetes is held by every namespace until the namespace
// controller has removed its contents.
const FinalizerKubernetes = "kubernetes"

// Node is a worker registered by its kubelet.
type Node struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              NodeSpec   `json:"spec,omitzero"`
	Status            NodeStatus `json:"status,omitzero"`
}

// NodeSpec holds scheduling controls.
type NodeSpec struct {
	// PodCIDR is the node's Pod address range, assigned once by the node IPAM controller.
	PodCIDR       string `json:"podCIDR,omitempty"`
	Unschedulable bool   `json:"unschedulable,omitempty"`
	// Taints repel Pods that do not tolerate them.
	Taints []Taint `json:"taints,omitempty"`
}

// NodeStatus is reported by the kubelet.
type NodeStatus struct {
	// Capacity is what the node has; Allocatable what Pods may use.
	Capacity    ResourceList    `json:"capacity,omitempty"`
	Allocatable ResourceList    `json:"allocatable,omitempty"`
	Conditions  []NodeCondition `json:"conditions,omitempty"`
	Addresses   []NodeAddress   `json:"addresses,omitempty"`
	NodeInfo    NodeSystemInfo  `json:"nodeInfo,omitzero"`
}

// NodeCondition is one aspect of node health.
type NodeCondition struct {
	Type               string      `json:"type"`
	Status             string      `json:"status"`
	LastHeartbeatTime  metav1.Time `json:"lastHeartbeatTime,omitzero"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitzero"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
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
	NodeReady        = "Ready"
	NodeInternalIP   = "InternalIP"
	NodeHostName     = "Hostname"
	ConditionTrue    = "True"
	ConditionFalse   = "False"
	ConditionUnknown = "Unknown"
	LabelHostname    = "kubernetes.io/hostname"
	LabelOSStable    = "kubernetes.io/os"
	LabelArchStable  = "kubernetes.io/arch"
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
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              PodSpec   `json:"spec"`
	Status            PodStatus `json:"status,omitzero"`
}

// PodSpec is the desired state of a Pod.
type PodSpec struct {
	// InitContainers run one after another, each to successful completion,
	// before the containers start.
	InitContainers []Container `json:"initContainers,omitempty"`
	Containers     []Container `json:"containers"`
	// Volumes can be mounted by the Pod's containers.
	Volumes []Volume `json:"volumes,omitempty"`
	// RestartPolicy is Always (default), OnFailure or Never.
	RestartPolicy                 string `json:"restartPolicy,omitempty"`
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`
	NodeName                      string `json:"nodeName,omitempty"`
	// HostNetwork runs the Pod in the node's network namespace.
	HostNetwork bool `json:"hostNetwork,omitempty"`
	// Tolerations let the Pod run on nodes with matching taints.
	Tolerations []Toleration `json:"tolerations,omitempty"`
}

// Annotations of static Pods and their mirror Pods.
const (
	// AnnotationConfigMirror marks a mirror Pod; its value is the static
	// Pod's hash.
	AnnotationConfigMirror = "kubernetes.io/config.mirror"
	// AnnotationConfigSource is where a Pod came from: "api" or "file".
	AnnotationConfigSource = "kubernetes.io/config.source"
	// AnnotationConfigHash is the static Pod's hash (its UID).
	AnnotationConfigHash = "kubernetes.io/config.hash"
)

// IsMirrorPod reports whether p mirrors a static Pod.
func IsMirrorPod(p *Pod) bool {
	_, ok := p.Annotations[AnnotationConfigMirror]
	return ok
}

// Image pull policies.
const (
	PullAlways       = "Always"
	PullIfNotPresent = "IfNotPresent"
	PullNever        = "Never"
)

// Volume is a named directory available to the Pod's containers; exactly
// one source is set.
type Volume struct {
	Name         string `json:"name"`
	VolumeSource `json:",inline"`
}

// VolumeSource is where a volume's data lives.
type VolumeSource struct {
	EmptyDir *EmptyDirVolumeSource `json:"emptyDir,omitempty"`
	HostPath *HostPathVolumeSource `json:"hostPath,omitempty"`
}

// EmptyDirVolumeSource is a directory that lives as long as the Pod on its node.
type EmptyDirVolumeSource struct {
	// Medium is "" (the node's disk) or "Memory" (tmpfs).
	Medium string `json:"medium,omitempty"`
	// SizeLimit sizes a Memory volume.
	SizeLimit *resource.Quantity `json:"sizeLimit,omitempty"`
}

// HostPathVolumeSource is a file or directory of the node.
type HostPathVolumeSource struct {
	Path string `json:"path"`
	// Type is checked before mounting: "" (no check), DirectoryOrCreate,
	// Directory, FileOrCreate or File.
	Type *string `json:"type,omitempty"`
}

// Storage media and hostPath types.
const (
	StorageMediumMemory       = "Memory"
	HostPathDirectoryOrCreate = "DirectoryOrCreate"
	HostPathDirectory         = "Directory"
	HostPathFileOrCreate      = "FileOrCreate"
	HostPathFile              = "File"
)

// VolumeMount mounts a Pod volume into a container.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// Restart policies.
const (
	RestartPolicyAlways    = "Always"
	RestartPolicyOnFailure = "OnFailure"
	RestartPolicyNever     = "Never"
)

// Container is one process image in a Pod.
type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	// ImagePullPolicy is Always, IfNotPresent or Never.
	ImagePullPolicy string               `json:"imagePullPolicy,omitempty"`
	Command         []string             `json:"command,omitempty"`
	Args            []string             `json:"args,omitempty"`
	Env             []EnvVar             `json:"env,omitempty"`
	Ports           []ContainerPort      `json:"ports,omitempty"`
	Resources       ResourceRequirements `json:"resources,omitzero"`
	VolumeMounts    []VolumeMount        `json:"volumeMounts,omitempty"`
	// LivenessProbe failures restart the container.
	LivenessProbe *Probe `json:"livenessProbe,omitempty"`
	// ReadinessProbe failures take the container (and Pod) out of service.
	ReadinessProbe *Probe `json:"readinessProbe,omitempty"`
	// StartupProbe holds off the other probes until it succeeds once.
	StartupProbe *Probe `json:"startupProbe,omitempty"`
}

// Resource names.
const (
	ResourceCPU    = "cpu"
	ResourceMemory = "memory"
	ResourcePods   = "pods"
)

// ResourceList maps resource names to quantities.
type ResourceList map[string]resource.Quantity

// ResourceRequirements are a container's requested (guaranteed, used for
// CPU shares) and limited (enforced by cgroups) resources.
type ResourceRequirements struct {
	Limits   ResourceList `json:"limits,omitempty"`
	Requests ResourceList `json:"requests,omitempty"`
}

// Probe checks a container periodically.
type Probe struct {
	ProbeHandler        `json:",inline"`
	InitialDelaySeconds int32 `json:"initialDelaySeconds,omitempty"`
	TimeoutSeconds      int32 `json:"timeoutSeconds,omitempty"`
	PeriodSeconds       int32 `json:"periodSeconds,omitempty"`
	SuccessThreshold    int32 `json:"successThreshold,omitempty"`
	FailureThreshold    int32 `json:"failureThreshold,omitempty"`
}

// ProbeHandler is the check a probe runs; exactly one member is set.
type ProbeHandler struct {
	Exec      *ExecAction      `json:"exec,omitempty"`
	HTTPGet   *HTTPGetAction   `json:"httpGet,omitempty"`
	TCPSocket *TCPSocketAction `json:"tcpSocket,omitempty"`
}

// ExecAction runs a command in the container; exit code 0 is success.
type ExecAction struct {
	Command []string `json:"command,omitempty"`
}

// HTTPGetAction sends a GET; status codes 200-399 are success.
type HTTPGetAction struct {
	Path string `json:"path,omitempty"`
	// Port is a number or the name of a container port.
	Port intstr.IntOrString `json:"port"`
	// Host defaults to the Pod IP.
	Host        string       `json:"host,omitempty"`
	Scheme      string       `json:"scheme,omitempty"`
	HTTPHeaders []HTTPHeader `json:"httpHeaders,omitempty"`
}

// HTTPHeader is a header sent by an HTTP probe.
type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// TCPSocketAction opens a TCP connection; connecting is success.
type TCPSocketAction struct {
	Port intstr.IntOrString `json:"port"`
	Host string             `json:"host,omitempty"`
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
	Phase                 string            `json:"phase,omitempty"`
	Conditions            []PodCondition    `json:"conditions,omitempty"`
	Message               string            `json:"message,omitempty"`
	Reason                string            `json:"reason,omitempty"`
	HostIP                string            `json:"hostIP,omitempty"`
	PodIP                 string            `json:"podIP,omitempty"`
	StartTime             *metav1.Time      `json:"startTime,omitempty"`
	InitContainerStatuses []ContainerStatus `json:"initContainerStatuses,omitempty"`
	ContainerStatuses     []ContainerStatus `json:"containerStatuses,omitempty"`
	// QOSClass is set when the Pod is created: Guaranteed, Burstable or BestEffort.
	QOSClass string `json:"qosClass,omitempty"`
}

// QoS classes.
const (
	PodQOSGuaranteed = "Guaranteed"
	PodQOSBurstable  = "Burstable"
	PodQOSBestEffort = "BestEffort"
)

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
	Type               string      `json:"type"`
	Status             string      `json:"status"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitzero"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
}

// ContainerStatus reports one container.
type ContainerStatus struct {
	Name                 string         `json:"name"`
	State                ContainerState `json:"state,omitzero"`
	LastTerminationState ContainerState `json:"lastState,omitzero"`
	Ready                bool           `json:"ready"`
	RestartCount         int32          `json:"restartCount"`
	// Started reports whether the container passed its startup probe (true
	// once running when it has none).
	Started     *bool  `json:"started,omitempty"`
	Image       string `json:"image"`
	ImageID     string `json:"imageID,omitempty"`
	ContainerID string `json:"containerID,omitempty"`
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
	StartedAt metav1.Time `json:"startedAt,omitzero"`
}

// ContainerStateTerminated records how a container exited.
type ContainerStateTerminated struct {
	ExitCode    int32       `json:"exitCode"`
	Reason      string      `json:"reason,omitempty"`
	Message     string      `json:"message,omitempty"`
	StartedAt   metav1.Time `json:"startedAt,omitzero"`
	FinishedAt  metav1.Time `json:"finishedAt,omitzero"`
	ContainerID string      `json:"containerID,omitempty"`
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
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              ServiceSpec   `json:"spec"`
	Status            ServiceStatus `json:"status,omitzero"`
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
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	InvolvedObject    ObjectReference `json:"involvedObject"`
	Reason            string          `json:"reason,omitempty"`
	Message           string          `json:"message,omitempty"`
	Source            EventSource     `json:"source,omitzero"`
	FirstTimestamp    metav1.Time     `json:"firstTimestamp,omitzero"`
	LastTimestamp     metav1.Time     `json:"lastTimestamp,omitzero"`
	Count             int32           `json:"count,omitempty"`
	Type              string          `json:"type,omitempty"`
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
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Target            ObjectReference `json:"target"`
}

// PodTemplateSpec describes Pods created by a controller.
type PodTemplateSpec struct {
	metav1.ObjectMeta `json:"metadata"`
	Spec              PodSpec `json:"spec"`
}

// NodeHeartbeatGrace is how stale a Ready heartbeat may be before the node is
// treated as not ready by consumers that read Node status directly.
const NodeHeartbeatGrace = 20 * time.Second

// IsNodeReady reports whether the node's Ready condition is True and fresh.
func IsNodeReady(n *Node, now time.Time) bool {
	c := n.Status.Condition(NodeReady)
	return c != nil && c.Status == ConditionTrue && now.Sub(c.LastHeartbeatTime.Time) <= NodeHeartbeatGrace
}

// IsPodTerminal reports whether the Pod has finished for good.
func IsPodTerminal(p *Pod) bool {
	return p.Status.Phase == PodSucceeded || p.Status.Phase == PodFailed
}

// PodExecResult is the outcome of a command run in a container through the
// Pod exec subresource. Kubernetes streams exec sessions over SPDY or
// WebSocket; mini-k8s runs non-interactive commands to completion instead.
type PodExecResult struct {
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
	ExitCode int32  `json:"exitCode"`
}
