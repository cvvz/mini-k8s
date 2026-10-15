// Package kubelet is the node agent: it registers its Node and keeps it
// heartbeating, configures Pod networking (CNI configuration and host-gw
// routes), runs the Pods bound to it through a CRI runtime, reports their
// status, terminates deleted Pods gracefully and serves container logs.
package kubelet

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"mini-k8s/pkg/api/resource"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/kubelet/envvars"
	"mini-k8s/pkg/kubelet/kuberuntime"
	"mini-k8s/pkg/kubelet/prober"
	"mini-k8s/pkg/network/hostgw"
	"mini-k8s/pkg/util/deepcopy"
	"mini-k8s/pkg/util/ptr"
)

// Version is reported in NodeInfo.
const Version = "mini-kubelet/v2"

// Runtime is the container runtime as seen by the kubelet (implemented by
// kuberuntime.Manager over CRI).
type Runtime interface {
	SyncPod(ctx context.Context, pod *corev1.Pod) corev1.PodStatus
	KillPod(ctx context.Context, pod *corev1.Pod, grace int64) error
	Cleanup(ctx context.Context, keep func() map[string]bool) error
	Logs(ctx context.Context, pod *corev1.Pod, container string, o kuberuntime.LogOptions, w io.Writer) error
	Exec(ctx context.Context, pod *corev1.Pod, container string, cmd []string, timeout time.Duration) (*corev1.PodExecResult, error)
	StopContainer(ctx context.Context, pod *corev1.Pod, containerID string, grace int64) error
	Version(ctx context.Context) (string, error)
	Status(ctx context.Context, sandboxImage string) kuberuntime.Readiness
}

// Config describes the node a kubelet runs on.
type Config struct {
	NodeName string
	// Address is the node's InternalIP.
	Address string
	// Token authenticates the API server when it fetches logs.
	Token string
	// SandboxImage must be present before the node reports Ready.
	SandboxImage string
	// CNIConfDir receives the generated CNI configuration.
	CNIConfDir string
	// Routes, when set, keeps host-gw routes to other nodes' Pod subnets.
	Routes *hostgw.Agent
	// StaticPodPath is a directory of Pod manifests run without the API.
	StaticPodPath string
	// Role, such as "control-plane" or "worker", becomes the
	// node-role.kubernetes.io/<role> label when the node registers; a
	// control-plane node also gets the control-plane NoSchedule taint.
	Role string
}

// Kubelet manages the Pods of one node.
type Kubelet struct {
	cfg      Config
	client   *client.Client
	runtime  Runtime
	recorder *record.Recorder
	pods     *informers.Informer[corev1.Pod, *corev1.Pod]
	nodes    *informers.Informer[corev1.Node, *corev1.Node]
	services *informers.Informer[corev1.Service, *corev1.Service]
	network  chan struct{}
	probes   *prober.Manager

	mu      sync.Mutex
	workers map[string]chan struct{} // by Pod UID
	killing map[string]bool
	static  map[string]*corev1.Pod // static Pods by UID
}

// New returns a kubelet for cfg.NodeName.
func New(c *client.Client, rt Runtime, recorder *record.Recorder, cfg Config) *Kubelet {
	k := &Kubelet{cfg: cfg, client: c, runtime: rt, recorder: recorder, network: make(chan struct{}, 1),
		workers: map[string]chan struct{}{}, killing: map[string]bool{}, static: map[string]*corev1.Pod{}}
	k.probes = prober.NewManager(rt.Exec, recorder)
	k.probes.Changed = k.dispatch
	k.probes.Failed = k.restartContainer
	k.pods = informers.New(client.Pods(c, ""), client.ListOptions{FieldSelector: "spec.nodeName=" + cfg.NodeName}, 0)
	// Mirror Pods are not run; their changes wake the static Pod they mirror.
	onPod := func(p *corev1.Pod) {
		if corev1.IsMirrorPod(p) {
			if static, ok := k.staticPodNamed(p.Namespace, p.Name); ok {
				k.dispatch(static)
			}
			return
		}
		k.dispatch(p)
	}
	k.pods.AddHandler(informers.Handler[*corev1.Pod]{
		OnAdd:    onPod,
		OnUpdate: func(_, p *corev1.Pod) { onPod(p) },
		OnDelete: onPod,
	})
	poke := func() {
		select {
		case k.network <- struct{}{}:
		default:
		}
	}
	k.services = informers.New(client.Services(c, ""), client.ListOptions{}, 0)
	k.nodes = informers.New(client.Nodes(c), client.ListOptions{}, 0)
	k.nodes.AddHandler(informers.Handler[*corev1.Node]{
		OnAdd: func(*corev1.Node) { poke() }, OnUpdate: func(_, _ *corev1.Node) { poke() }, OnDelete: func(*corev1.Node) { poke() },
	})
	return k
}

// Probes returns the kubelet's probe results, for the runtime.
func (k *Kubelet) Probes() *prober.Manager { return k.probes }

// restartContainer stops a container whose liveness or startup probe failed;
// the Pod's worker then restarts it according to the restart policy.
func (k *Kubelet) restartContainer(pod *corev1.Pod, containerID, message string) {
	k.recorder.Eventf(pod, corev1.EventTypeNormal, "Killing", "%s", message)
	grace := int64(30)
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		grace = *pod.Spec.TerminationGracePeriodSeconds
	}
	go func() {
		if err := k.runtime.StopContainer(context.Background(), pod, containerID, grace); err != nil {
			log.Printf("kubelet: stop %s of %s/%s: %v", containerID, pod.Namespace, pod.Name, err)
		}
		k.dispatch(pod)
	}()
}

// Run registers the node and reconciles Pods until ctx ends.
func (k *Kubelet) Run(ctx context.Context) {
	go k.heartbeatLoop(ctx)
	go k.nodes.Run(ctx)
	go k.networkLoop(ctx)
	go k.pods.Run(ctx)
	go k.services.Run(ctx)
	// Static Pods run without the API, which may itself be one of them.
	if k.cfg.StaticPodPath != "" {
		go k.staticLoop(ctx)
	}
	// Containers get the variables of the Services that exist when they start.
	if !informers.WaitForSync(ctx, k.pods.HasSynced, k.services.HasSynced) {
		return
	}
	poll := time.NewTicker(time.Second)
	housekeeping := time.NewTicker(10 * time.Second)
	defer poll.Stop()
	defer housekeeping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			// Container exits are not pushed to us; poll each Pod's state.
			for _, p := range k.pods.List("", nil) {
				if !corev1.IsMirrorPod(p) {
					k.dispatch(p)
				}
			}
		case <-housekeeping.C:
			k.cleanupOrphans(ctx)
		}
	}
}

// networkLoop writes the CNI configuration once the node has a Pod CIDR and
// keeps host-gw routes in line with the node list.
func (k *Kubelet) networkLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if k.nodes.HasSynced() {
			nodes := k.nodes.List("", nil)
			for _, n := range nodes {
				if n.Name == k.cfg.NodeName && n.Spec.PodCIDR != "" {
					if err := WriteCNIConfig(k.cfg.CNIConfDir, n.Spec.PodCIDR); err != nil {
						log.Printf("kubelet: write CNI config: %v", err)
					}
				}
			}
			if k.cfg.Routes != nil {
				if err := k.cfg.Routes.Sync(nodes); err != nil {
					log.Printf("kubelet: sync routes: %v", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-k.network:
		case <-ticker.C:
		}
	}
}

// CNIConfigFile is the network configuration containerd loads.
const CNIConfigFile = "10-mini.conflist"

// WriteCNIConfig writes the mini-cni configuration for podCIDR, leaving an
// identical file untouched so containerd does not reload needlessly.
func WriteCNIConfig(dir, podCIDR string) error {
	data, err := json.MarshalIndent(map[string]any{
		"cniVersion": "1.0.0", "name": "mini",
		"plugins": []any{map[string]any{"type": "mini-cni", "bridge": "cni0", "subnet": podCIDR}},
	}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, CNIConfigFile)
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// dispatch wakes (or starts) the worker that owns the Pod's UID.
func (k *Kubelet) dispatch(p *corev1.Pod) {
	k.mu.Lock()
	defer k.mu.Unlock()
	ch, ok := k.workers[p.UID]
	if !ok {
		ch = make(chan struct{}, 1)
		k.workers[p.UID] = ch
		go k.worker(informers.Key(p), p.UID, ch)
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// worker serialises all work for one Pod, so a slow image pull never blocks
// other Pods.
func (k *Kubelet) worker(key, uid string, wake chan struct{}) {
	ns, name := informers.SplitKey(key)
	for range wake {
		pod, ok := k.staticPod(uid)
		if !ok {
			pod, ok = k.pods.Get(ns, name)
		}
		if !ok || pod.UID != uid || corev1.IsMirrorPod(pod) {
			k.probes.Remove(uid)
			k.mu.Lock()
			delete(k.workers, uid)
			delete(k.killing, uid)
			k.mu.Unlock()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		k.syncPod(ctx, pod)
		cancel()
	}
}

func (k *Kubelet) syncPod(ctx context.Context, pod *corev1.Pod) {
	if pod.DeletionTimestamp != nil {
		k.terminate(ctx, pod)
		return
	}
	status := k.runtime.SyncPod(ctx, pod)
	if pod.Spec.HostNetwork {
		status.PodIP = k.cfg.Address // the node's network namespace
	}
	k.probes.Sync(pod, status)
	status.HostIP = k.cfg.Address
	if status.StartTime == nil {
		now := metav1.Now()
		status.StartTime = &now
	}
	k.recordTransitions(pod, &status)
	if _, static := k.staticPod(pod.UID); static {
		k.syncStatic(ctx, pod, status)
		return
	}
	if equalStatus(pod.Status, status) {
		return
	}
	cp := deepcopy.Copy(pod)
	cp.Status = status
	if _, err := client.Pods(k.client, pod.Namespace).UpdateStatus(ctx, cp); err != nil && !client.IsConflict(err) && !client.IsNotFound(err) {
		log.Printf("kubelet: update status of %s/%s: %v", pod.Namespace, pod.Name, err)
	}
}

func equalStatus(a, b corev1.PodStatus) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return bytes.Equal(aj, bj)
}

// recordTransitions emits Events for container state changes.
func (k *Kubelet) recordTransitions(pod *corev1.Pod, status *corev1.PodStatus) {
	k.recordContainerTransitions(pod, pod.Status.InitContainerStatuses, status.InitContainerStatuses)
	k.recordContainerTransitions(pod, pod.Status.ContainerStatuses, status.ContainerStatuses)
}

func (k *Kubelet) recordContainerTransitions(pod *corev1.Pod, before, after []corev1.ContainerStatus) {
	for _, cs := range after {
		prev := containerStatus(before, cs.Name)
		switch {
		case cs.State.Running != nil && (prev == nil || prev.State.Running == nil || prev.RestartCount != cs.RestartCount):
			k.recorder.Eventf(pod, corev1.EventTypeNormal, "Started", "Started container %s", cs.Name)
		case cs.State.Waiting != nil && (prev == nil || prev.State.Waiting == nil || prev.State.Waiting.Reason != cs.State.Waiting.Reason):
			switch cs.State.Waiting.Reason {
			case kuberuntime.ReasonCrashLoopBackOff:
				k.recorder.Eventf(pod, corev1.EventTypeWarning, "BackOff", "Back-off restarting failed container %s", cs.Name)
			case kuberuntime.ReasonErrImagePull, kuberuntime.ReasonImagePullBackOff, kuberuntime.ReasonCreateContainerError,
				kuberuntime.ReasonCreateContainerConfigError, kuberuntime.ReasonErrImageNeverPull,
				kuberuntime.ReasonRunContainerError, kuberuntime.ReasonCreatePodSandboxError:
				k.recorder.Eventf(pod, corev1.EventTypeWarning, "Failed", "%s: %s", cs.State.Waiting.Reason, cs.State.Waiting.Message)
			}
		}
	}
}

// terminate stops the Pod's containers within its grace period and then
// completes the deletion in the API.
func (k *Kubelet) terminate(ctx context.Context, pod *corev1.Pod) {
	grace := int64(30)
	if pod.DeletionGracePeriodSeconds != nil {
		grace = *pod.DeletionGracePeriodSeconds
	}
	k.probes.Remove(pod.UID)
	k.mu.Lock()
	first := !k.killing[pod.UID]
	k.killing[pod.UID] = true
	k.mu.Unlock()
	if first {
		for _, cs := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
			if cs.State.Running != nil {
				k.recorder.Eventf(pod, corev1.EventTypeNormal, "Killing", "Stopping container %s (grace period %ds)", cs.Name, grace)
			}
		}
	}
	if err := k.runtime.KillPod(ctx, pod, grace); err != nil {
		log.Printf("kubelet: kill %s/%s: %v", pod.Namespace, pod.Name, err)
		return
	}
	if _, static := k.staticPod(pod.UID); static {
		k.removeStatic(ctx, pod)
		return
	}
	err := client.Pods(k.client, pod.Namespace).Delete(ctx, pod.Name, &metav1.DeleteOptions{
		GracePeriodSeconds: ptr.To[int64](0), Preconditions: &metav1.Preconditions{UID: &pod.UID}})
	if err != nil && !client.IsNotFound(err) && !client.IsConflict(err) {
		log.Printf("kubelet: finish deletion of %s/%s: %v", pod.Namespace, pod.Name, err)
	}
}

// cleanupOrphans removes what belongs to Pods that are neither in the API
// nor still being handled by a worker.
func (k *Kubelet) cleanupOrphans(ctx context.Context) {
	keep := func() map[string]bool {
		uids := map[string]bool{}
		for _, p := range k.pods.List("", nil) {
			uids[p.UID] = true
		}
		k.mu.Lock()
		for uid := range k.workers {
			uids[uid] = true
		}
		for uid := range k.static {
			uids[uid] = true
		}
		k.mu.Unlock()
		return uids
	}
	if err := k.runtime.Cleanup(ctx, keep); err != nil {
		log.Printf("kubelet: cleanup: %v", err)
	}
	// Mirror Pods of static Pods this kubelet no longer runs.
	for _, p := range k.pods.List("", nil) {
		if !corev1.IsMirrorPod(p) {
			continue
		}
		if _, ok := k.staticPod(p.Annotations[corev1.AnnotationConfigMirror]); ok {
			continue
		}
		err := client.Pods(k.client, p.Namespace).Delete(ctx, p.Name, &metav1.DeleteOptions{
			GracePeriodSeconds: ptr.To[int64](0), Preconditions: &metav1.Preconditions{UID: &p.UID}})
		if err != nil && !client.IsNotFound(err) && !client.IsConflict(err) {
			log.Printf("kubelet: delete orphaned mirror Pod %s/%s: %v", p.Namespace, p.Name, err)
		}
	}
}

func (k *Kubelet) heartbeatLoop(ctx context.Context) {
	for {
		if err := k.heartbeat(ctx); err != nil && ctx.Err() == nil {
			log.Printf("kubelet: heartbeat: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// heartbeat registers the Node if needed and refreshes its Ready condition,
// which is True only when the runtime, its network plugin and the sandbox
// image are ready.
func (k *Kubelet) heartbeat(ctx context.Context) error {
	nodes := client.Nodes(k.client)
	node, err := nodes.Get(ctx, k.cfg.NodeName)
	if client.IsNotFound(err) {
		labels := map[string]string{
			corev1.LabelHostname: k.cfg.NodeName, corev1.LabelOSStable: "linux", corev1.LabelArchStable: runtime.GOARCH,
		}
		var taints []corev1.Taint
		if k.cfg.Role != "" {
			labels[corev1.LabelNodeRolePrefix+k.cfg.Role] = ""
		}
		if k.cfg.Role == "control-plane" {
			taints = append(taints, corev1.Taint{Key: corev1.TaintNodeRoleControlPlane, Effect: corev1.TaintEffectNoSchedule})
		}
		node, err = nodes.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: k.cfg.NodeName, Labels: labels},
			Spec: corev1.NodeSpec{Taints: taints}})
	}
	if err != nil {
		return err
	}
	readiness := k.runtime.Status(ctx, k.cfg.SandboxImage)
	now := metav1.Now()
	ready := corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady",
		Message: "kubelet is posting ready status", LastHeartbeatTime: now, LastTransitionTime: now}
	if !readiness.Ready {
		ready.Status, ready.Reason, ready.Message = corev1.ConditionFalse, "KubeletNotReady", readiness.Message
	}
	if prev := node.Status.Condition(corev1.NodeReady); prev != nil && prev.Status == ready.Status {
		ready.LastTransitionTime = prev.LastTransitionTime
	}
	runtimeVersion, _ := k.runtime.Version(ctx)
	capacity := NodeCapacity("/proc/meminfo")
	node.Status = corev1.NodeStatus{
		Capacity: capacity, Allocatable: capacity,
		Conditions: []corev1.NodeCondition{ready},
		Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: k.cfg.Address}, {Type: corev1.NodeHostName, Address: k.cfg.NodeName}},
		NodeInfo: corev1.NodeSystemInfo{KubeletVersion: Version, ContainerRuntimeVersion: runtimeVersion,
			OperatingSystem: "linux", Architecture: runtime.GOARCH},
	}
	_, err = nodes.UpdateStatus(ctx, node)
	return err
}

// Exec timeouts: the default, and the most a request may ask for.
const (
	DefaultExecTimeout = time.Minute
	MaxExecTimeout     = time.Hour
)

// MaxPods is the number of Pods a node reports it can hold.
const MaxPods = 110

// NodeCapacity reports the node's CPUs, memory (MemTotal of meminfo) and
// Pod capacity. Nothing is reserved for the system, so allocatable equals
// capacity.
func NodeCapacity(meminfo string) corev1.ResourceList {
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:  resource.NewQuantity(int64(runtime.NumCPU())),
		corev1.ResourcePods: resource.NewQuantity(MaxPods),
	}
	data, err := os.ReadFile(meminfo)
	if err != nil {
		return capacity
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			if q, err := resource.ParseQuantity(fields[1] + "Ki"); err == nil {
				capacity[corev1.ResourceMemory] = q
			}
		}
	}
	return capacity
}

// Handler serves the kubelet API:
//
//	GET  /healthz
//	GET  /containerLogs/{namespace}/{pod}/{container}?follow=&tailLines=&previous=
//	POST /exec/{namespace}/{pod}/{container}?command=...&timeout=SECONDS
func (k *Kubelet) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+k.cfg.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		route, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		parts := strings.Split(rest, "/")
		method := map[string]string{"containerLogs": http.MethodGet, "exec": http.MethodPost}[route]
		if method == "" || r.Method != method || len(parts) != 3 {
			http.NotFound(w, r)
			return
		}
		pod, ok := k.staticPodNamed(parts[0], parts[1])
		if !ok {
			pod, ok = k.pods.Get(parts[0], parts[1])
		}
		if !ok {
			http.Error(w, "pod not found on this node", http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		if route == "exec" {
			k.serveExec(w, r, pod, parts[2], q)
			return
		}
		tail, _ := strconv.Atoi(q.Get("tailLines"))
		opts := kuberuntime.LogOptions{Follow: q.Get("follow") == "true", TailLines: tail, Previous: q.Get("previous") == "true"}
		w.Header().Set("Content-Type", "text/plain")
		if err := k.runtime.Logs(r.Context(), pod, parts[2], opts, w); err != nil {
			log.Printf("kubelet: logs %s/%s/%s: %v", parts[0], parts[1], parts[2], err)
			http.Error(w, err.Error(), http.StatusNotFound)
		}
	})
}

func (k *Kubelet) serveExec(w http.ResponseWriter, r *http.Request, pod *corev1.Pod, container string, q url.Values) {
	timeout := DefaultExecTimeout
	if s, err := strconv.Atoi(q.Get("timeout")); err == nil && s > 0 && s <= int(MaxExecTimeout/time.Second) {
		timeout = time.Duration(s) * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout+10*time.Second)
	defer cancel()
	res, err := k.runtime.Exec(ctx, pod, container, q["command"], timeout)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// containerStatus returns the status of the named container, if reported.
func containerStatus(list []corev1.ContainerStatus, name string) *corev1.ContainerStatus {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

// ServiceEnv returns the Service environment variables of a Pod's containers.
func (k *Kubelet) ServiceEnv(pod *corev1.Pod) []corev1.EnvVar {
	return envvars.ForPod(pod, k.services.List("", nil))
}
