// Package v1 contains the core API group types (corev1): Namespace, Node,
// Pod, Service, Endpoints, Event and Binding.
package v1

import (
	"fmt"

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
	// NamespaceNodeLease holds the kubelets' Leases.
	NamespaceNodeLease = "kube-node-lease"
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
	// Hostname is the Pod's host name (default: its name); with Subdomain
	// set to a headless Service's name the Pod gets the DNS name
	// <hostname>.<subdomain>.<namespace>.svc.<cluster domain>.
	Hostname  string `json:"hostname,omitempty"`
	Subdomain string `json:"subdomain,omitempty"`
	// DNSPolicy is ClusterFirst (default: the cluster DNS), ClusterFirstWithHostNet,
	// Default (the node's resolver) or None (only DNSConfig).
	DNSPolicy string        `json:"dnsPolicy,omitempty"`
	DNSConfig *PodDNSConfig `json:"dnsConfig,omitempty"`
	// EnableServiceLinks injects the environment variables of the Services
	// in the Pod's namespace (default true).
	EnableServiceLinks *bool `json:"enableServiceLinks,omitempty"`
	// NodeSelector limits the Pod to nodes with all these labels.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Affinity holds node and inter-Pod (anti-)affinity rules.
	Affinity *Affinity `json:"affinity,omitempty"`
}

// DNS policies.
const (
	DNSClusterFirstWithHostNet = "ClusterFirstWithHostNet"
	DNSClusterFirst            = "ClusterFirst"
	DNSDefault                 = "Default"
	DNSNone                    = "None"
)

// PodDNSConfig is merged into the resolver configuration the policy yields.
type PodDNSConfig struct {
	Nameservers []string             `json:"nameservers,omitempty"`
	Searches    []string             `json:"searches,omitempty"`
	Options     []PodDNSConfigOption `json:"options,omitempty"`
}

// PodDNSConfigOption is a resolv.conf option such as ndots:2.
type PodDNSConfigOption struct {
	Name  string  `json:"name"`
	Value *string `json:"value,omitempty"`
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
	// SecurityContext grants extra privileges, e.g. to kube-proxy.
	SecurityContext *SecurityContext `json:"securityContext,omitempty"`
}

// SecurityContext holds a container's privileges.
type SecurityContext struct {
	// Privileged gives the container all capabilities and devices of the node.
	Privileged   *bool         `json:"privileged,omitempty"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// Capabilities adds or drops Linux capabilities such as NET_ADMIN.
type Capabilities struct {
	Add  []string `json:"add,omitempty"`
	Drop []string `json:"drop,omitempty"`
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
	// DisruptionTarget marks a Pod about to be deleted by a disruption,
	// such as the Pod garbage collector removing it from a vanished node.
	DisruptionTarget = "DisruptionTarget"
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

// IsServiceIPSet reports whether a Service has a cluster IP (not headless,
// not ExternalName).
func IsServiceIPSet(s *Service) bool {
	return s.Spec.ClusterIP != "" && s.Spec.ClusterIP != ClusterIPNone
}

// FindPort resolves a Service port's targetPort for a Pod: a number is used
// as is, a name is looked up among the container ports with the same
// protocol (like FindPort in k8s.io/kubernetes/pkg/api/v1/pod).
func FindPort(p *Pod, port *ServicePort) (int32, error) {
	if !port.TargetPort.IsString {
		return port.TargetPort.IntVal, nil
	}
	for _, c := range p.Spec.Containers {
		for _, cp := range c.Ports {
			protocol := cp.Protocol
			if protocol == "" {
				protocol = ProtocolTCP
			}
			if cp.Name == port.TargetPort.StrVal && protocol == port.Protocol {
				return cp.ContainerPort, nil
			}
		}
	}
	return 0, fmt.Errorf("no suitable port for manifest: %s", p.UID)
}

// Service exposes Pods selected by labels.
type Service struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              ServiceSpec   `json:"spec"`
	Status            ServiceStatus `json:"status,omitzero"`
}

// Service types.
const (
	ServiceTypeClusterIP    = "ClusterIP"
	ServiceTypeNodePort     = "NodePort"
	ServiceTypeExternalName = "ExternalName"
	// ClusterIPNone makes a Service headless: DNS returns its endpoints.
	ClusterIPNone = "None"
)

// Service session affinities.
const (
	ServiceAffinityNone     = "None"
	ServiceAffinityClientIP = "ClientIP"
	// DefaultClientIPServiceAffinitySeconds is three hours, as in Kubernetes.
	DefaultClientIPServiceAffinitySeconds int32 = 10800
)

// Protocols.
const (
	ProtocolTCP = "TCP"
	ProtocolUDP = "UDP"
)

// ServiceSpec selects backends and ports.
type ServiceSpec struct {
	// Type is ClusterIP (the default), NodePort or ExternalName (a DNS
	// alias for ExternalName).
	Type string `json:"type,omitempty"`
	// Selector picks the backend Pods; without one the Endpoints are
	// managed by the user.
	Selector map[string]string `json:"selector,omitempty"`
	Ports    []ServicePort     `json:"ports"`
	// ClusterIP is allocated from the Service CIDR unless given; it never
	// changes afterwards. "None" makes the Service headless.
	ClusterIP string `json:"clusterIP,omitempty"`
	// ExternalName is the DNS name an ExternalName Service is an alias for.
	ExternalName string `json:"externalName,omitempty"`
	// PublishNotReadyAddresses lists every Pod as ready in the Endpoints,
	// for peers that must find each other before they are ready.
	PublishNotReadyAddresses bool `json:"publishNotReadyAddresses,omitempty"`
	// SessionAffinity is None or ClientIP (each client sticks to one backend).
	SessionAffinity       string                 `json:"sessionAffinity,omitempty"`
	SessionAffinityConfig *SessionAffinityConfig `json:"sessionAffinityConfig,omitempty"`
}

// SessionAffinityConfig tunes ClientIP affinity.
type SessionAffinityConfig struct {
	ClientIP *ClientIPConfig `json:"clientIP,omitempty"`
}

// ClientIPConfig holds how long a client stays with its backend.
type ClientIPConfig struct {
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`
}

// ServicePort maps a Service port to a port of the backends.
type ServicePort struct {
	// Name is required when the Service has more than one port; Endpoints
	// ports carry the same name.
	Name     string `json:"name,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Port     int32  `json:"port"`
	// TargetPort is a number or the name of a container port; it defaults
	// to Port.
	TargetPort intstr.IntOrString `json:"targetPort,omitzero"`
	// NodePort is opened on every node for NodePort Services; allocated
	// from 30000-32767 unless given.
	NodePort int32 `json:"nodePort,omitempty"`
}

// ServiceStatus mirrors Kubernetes' (always empty load balancer status).
type ServiceStatus struct {
	LoadBalancer LoadBalancerStatus `json:"loadBalancer"`
}

// LoadBalancerStatus lists load-balancer ingress points.
type LoadBalancerStatus struct {
	Ingress []LoadBalancerIngress `json:"ingress,omitempty"`
}

// LoadBalancerIngress is an address traffic for a Service or Ingress arrives on.
type LoadBalancerIngress struct {
	IP       string `json:"ip,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

// Endpoints lists the addresses behind a Service, maintained by the
// Endpoints controller for Services with a selector.
type Endpoints struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Subsets           []EndpointSubset `json:"subsets,omitempty"`
}

// EndpointSubset is a set of addresses sharing the same ports.
type EndpointSubset struct {
	Addresses         []EndpointAddress `json:"addresses,omitempty"`
	NotReadyAddresses []EndpointAddress `json:"notReadyAddresses,omitempty"`
	Ports             []EndpointPort    `json:"ports,omitempty"`
}

// EndpointAddress is one backend.
type EndpointAddress struct {
	IP        string           `json:"ip"`
	Hostname  string           `json:"hostname,omitempty"`
	NodeName  *string          `json:"nodeName,omitempty"`
	TargetRef *ObjectReference `json:"targetRef,omitempty"`
}

// EndpointPort is a port of the backends, named after the Service port.
type EndpointPort struct {
	Name     string `json:"name,omitempty"`
	Port     int32  `json:"port"`
	Protocol string `json:"protocol,omitempty"`
}

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

// IsNodeReady reports whether the node's Ready condition is True. A node
// that stops renewing its Lease is set to Unknown by the node lifecycle
// controller, so the condition is current.
func IsNodeReady(n *Node) bool {
	c := n.Status.Condition(NodeReady)
	return c != nil && c.Status == ConditionTrue
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
