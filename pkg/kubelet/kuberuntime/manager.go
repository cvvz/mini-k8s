// Package kuberuntime runs Pods through a CRI runtime (containerd), like
// Kubernetes' kuberuntime manager: a Pod becomes a sandbox (pause container
// plus network namespace, set up by CNI) holding the Pod's containers.
package kuberuntime

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// CRI labels identifying the objects of a Pod (the keys Kubernetes uses).
const (
	LabelPodUID        = "io.kubernetes.pod.uid"
	LabelPodName       = "io.kubernetes.pod.name"
	LabelPodNamespace  = "io.kubernetes.pod.namespace"
	LabelContainerName = "io.kubernetes.container.name"
)

// Waiting reasons reported in container statuses.
const (
	ReasonContainerCreating     = "ContainerCreating"
	ReasonErrImagePull          = "ErrImagePull"
	ReasonImagePullBackOff      = "ImagePullBackOff"
	ReasonCrashLoopBackOff      = "CrashLoopBackOff"
	ReasonRunContainerError     = "RunContainerError"
	ReasonCreateContainerError  = "CreateContainerError"
	ReasonCreatePodSandboxError = "CreatePodSandboxError"
)

// DefaultPodLogsDir is where container logs are written (by containerd).
const DefaultPodLogsDir = "/var/log/pods"

// Manager drives one CRI runtime.
type Manager struct {
	runtime runtimeapi.RuntimeServiceClient
	images  runtimeapi.ImageServiceClient
	// DNS is applied to every sandbox when set.
	DNS *runtimeapi.DNSConfig
	// PodLogsDir is the root of Pod log directories.
	PodLogsDir string
	now        func() time.Time

	mu          sync.Mutex
	restarts    map[string]backoff // by pod UID + container name
	pulls       map[string]backoff // by image
	sandboxRuns map[string]backoff // by pod UID
}

type backoff struct {
	attempts int
	next     time.Time
	err      error
}

// Dial connects to a CRI socket such as /run/containerd/containerd.sock.
func Dial(socket string) (*grpc.ClientConn, error) {
	return grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// NewManager returns a manager for the given CRI clients.
func NewManager(runtime runtimeapi.RuntimeServiceClient, images runtimeapi.ImageServiceClient) *Manager {
	return &Manager{runtime: runtime, images: images, PodLogsDir: DefaultPodLogsDir, now: time.Now,
		restarts: map[string]backoff{}, pulls: map[string]backoff{}, sandboxRuns: map[string]backoff{}}
}

// Version reports the runtime as "containerd://2.2.1".
func (m *Manager) Version(ctx context.Context) (string, error) {
	v, err := m.runtime.Version(ctx, &runtimeapi.VersionRequest{})
	if err != nil {
		return "", err
	}
	return v.RuntimeName + "://" + v.RuntimeVersion, nil
}

// Readiness summarises whether the node can run Pods: the runtime and its
// network plugin are ready and the sandbox image is present.
type Readiness struct {
	Ready   bool
	Message string
}

// Status checks runtime and network readiness and that sandboxImage exists.
func (m *Manager) Status(ctx context.Context, sandboxImage string) Readiness {
	resp, err := m.runtime.Status(ctx, &runtimeapi.StatusRequest{})
	if err != nil {
		return Readiness{Message: "container runtime unreachable: " + err.Error()}
	}
	var problems []string
	for _, c := range resp.GetStatus().GetConditions() {
		if !c.Status {
			problems = append(problems, fmt.Sprintf("%s=false reason:%s message:%s", c.Type, c.Reason, c.Message))
		}
	}
	if len(problems) > 0 {
		return Readiness{Message: "container runtime not ready: " + strings.Join(problems, "; ")}
	}
	if present, err := m.imagePresent(ctx, sandboxImage); err != nil || !present {
		return Readiness{Message: fmt.Sprintf("sandbox image %s is not present (err=%v)", sandboxImage, err)}
	}
	return Readiness{Ready: true}
}

func (m *Manager) imagePresent(ctx context.Context, image string) (bool, error) {
	resp, err := m.images.ImageStatus(ctx, &runtimeapi.ImageStatusRequest{Image: &runtimeapi.ImageSpec{Image: image}})
	if err != nil {
		return false, err
	}
	return resp.Image != nil, nil
}

func podLabels(pod *corev1.Pod) map[string]string {
	return map[string]string{LabelPodUID: pod.UID, LabelPodName: pod.Name, LabelPodNamespace: pod.Namespace}
}

// PodLogDir is the directory holding a Pod's container logs.
func (m *Manager) PodLogDir(pod *corev1.Pod) string {
	return fmt.Sprintf("%s/%s_%s_%s", m.PodLogsDir, pod.Namespace, pod.Name, pod.UID)
}

func (m *Manager) sandboxConfig(pod *corev1.Pod, attempt uint32) *runtimeapi.PodSandboxConfig {
	return &runtimeapi.PodSandboxConfig{
		Metadata:     &runtimeapi.PodSandboxMetadata{Name: pod.Name, Namespace: pod.Namespace, Uid: pod.UID, Attempt: attempt},
		Hostname:     podHostname(pod.Name),
		LogDirectory: m.PodLogDir(pod),
		DnsConfig:    m.DNS,
		Labels:       podLabels(pod),
		Linux:        &runtimeapi.LinuxPodSandboxConfig{},
	}
}

func containerConfig(pod *corev1.Pod, c corev1.Container, attempt uint32) *runtimeapi.ContainerConfig {
	labels := podLabels(pod)
	labels[LabelContainerName] = c.Name
	cfg := &runtimeapi.ContainerConfig{
		Metadata: &runtimeapi.ContainerMetadata{Name: c.Name, Attempt: attempt},
		Image:    &runtimeapi.ImageSpec{Image: c.Image},
		Command:  c.Command,
		Args:     c.Args,
		Labels:   labels,
		LogPath:  fmt.Sprintf("%s/%d.log", c.Name, attempt),
		Linux:    &runtimeapi.LinuxContainerConfig{},
	}
	for _, e := range c.Env {
		cfg.Envs = append(cfg.Envs, &runtimeapi.KeyValue{Key: e.Name, Value: []byte(e.Value)})
	}
	return cfg
}

// podHostname derives a valid hostname (at most 63 characters, not ending in
// '-' or '.') from the Pod name.
func podHostname(name string) string {
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-.")
}

// sandboxes lists the Pod's sandboxes, newest attempt first.
func (m *Manager) sandboxes(ctx context.Context, uid string) ([]*runtimeapi.PodSandbox, error) {
	resp, err := m.runtime.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{
		Filter: &runtimeapi.PodSandboxFilter{LabelSelector: map[string]string{LabelPodUID: uid}}})
	if err != nil {
		return nil, err
	}
	items := resp.Items
	sort.Slice(items, func(i, j int) bool {
		if items[i].Metadata.GetAttempt() != items[j].Metadata.GetAttempt() {
			return items[i].Metadata.GetAttempt() > items[j].Metadata.GetAttempt()
		}
		return items[i].CreatedAt > items[j].CreatedAt
	})
	return items, nil
}

// containers lists a Pod's instances of one container across its sandboxes,
// newest attempt first.
func (m *Manager) containers(ctx context.Context, uid, name string) ([]*runtimeapi.Container, error) {
	resp, err := m.runtime.ListContainers(ctx, &runtimeapi.ListContainersRequest{Filter: &runtimeapi.ContainerFilter{
		LabelSelector: map[string]string{LabelPodUID: uid, LabelContainerName: name}}})
	if err != nil {
		return nil, err
	}
	items := resp.Containers
	sort.Slice(items, func(i, j int) bool {
		if items[i].Metadata.GetAttempt() != items[j].Metadata.GetAttempt() {
			return items[i].Metadata.GetAttempt() > items[j].Metadata.GetAttempt()
		}
		return items[i].CreatedAt > items[j].CreatedAt
	})
	return items, nil
}

func (m *Manager) postpone(prev backoff, err error) backoff {
	if prev.attempts < 6 {
		prev.attempts++
	}
	delay := time.Second << (prev.attempts - 1)
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	prev.next, prev.err = m.now().Add(delay), err
	return prev
}
