// Package kubelet is the node agent: it registers its Node, keeps it
// heartbeating, runs the containers of Pods bound to it, reports their
// status, terminates deleted Pods gracefully and serves container logs.
package kubelet

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/kubelet/dockershim"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/pkg/util/deepcopy"
)

// Version is reported in NodeInfo.
const Version = "mini-kubelet/v2"

// Kubelet manages the Pods of one node.
type Kubelet struct {
	nodeName string
	address  string
	token    string
	client   *client.Client
	runtime  *dockershim.Runtime
	recorder *record.Recorder
	pods     *informers.Informer[corev1.Pod, *corev1.Pod]

	mu      sync.Mutex
	workers map[string]chan struct{} // by Pod UID
	killing map[string]bool
}

// New returns a kubelet for nodeName reachable at address.
func New(c *client.Client, rt *dockershim.Runtime, recorder *record.Recorder, nodeName, address, token string) *Kubelet {
	k := &Kubelet{nodeName: nodeName, address: address, token: token, client: c, runtime: rt, recorder: recorder,
		workers: map[string]chan struct{}{}, killing: map[string]bool{}}
	k.pods = informers.New(client.Pods(c, ""), client.ListOptions{FieldSelector: "spec.nodeName=" + nodeName}, 0)
	k.pods.AddHandler(informers.Handler[*corev1.Pod]{
		OnAdd:    func(p *corev1.Pod) { k.dispatch(p) },
		OnUpdate: func(_, p *corev1.Pod) { k.dispatch(p) },
		OnDelete: func(p *corev1.Pod) { k.dispatch(p) },
	})
	return k
}

// Run registers the node and reconciles Pods until ctx ends.
func (k *Kubelet) Run(ctx context.Context) {
	go k.heartbeatLoop(ctx)
	go k.pods.Run(ctx)
	if !informers.WaitForSync(ctx, k.pods.HasSynced) {
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
				k.dispatch(p)
			}
		case <-housekeeping.C:
			k.cleanupOrphans(ctx)
		}
	}
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
		pod, ok := k.pods.Get(ns, name)
		if !ok || pod.UID != uid {
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
	status.HostIP = k.address
	if status.StartTime == nil {
		now := metav1.Now()
		status.StartTime = &now
	}
	k.recordTransitions(pod, &status)
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
	for _, cs := range status.ContainerStatuses {
		prev := containerStatus(pod.Status.ContainerStatuses, cs.Name)
		switch {
		case cs.State.Running != nil && (prev == nil || prev.State.Running == nil || prev.RestartCount != cs.RestartCount):
			k.recorder.Eventf(pod, corev1.EventTypeNormal, "Started", "Started container %s", cs.Name)
		case cs.State.Waiting != nil && (prev == nil || prev.State.Waiting == nil || prev.State.Waiting.Reason != cs.State.Waiting.Reason):
			switch cs.State.Waiting.Reason {
			case dockershim.ReasonCrashLoopBackOff:
				k.recorder.Eventf(pod, corev1.EventTypeWarning, "BackOff", "Back-off restarting failed container %s", cs.Name)
			case dockershim.ReasonErrImagePull, dockershim.ReasonImagePullBackOff, dockershim.ReasonCreateError, dockershim.ReasonRunContainerError:
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
	k.mu.Lock()
	first := !k.killing[pod.UID]
	k.killing[pod.UID] = true
	k.mu.Unlock()
	if first {
		for _, c := range pod.Spec.Containers {
			k.recorder.Eventf(pod, corev1.EventTypeNormal, "Killing", "Stopping container %s (grace period %ds)", c.Name, grace)
		}
	}
	if err := k.runtime.KillPod(ctx, pod, grace); err != nil {
		log.Printf("kubelet: kill %s/%s: %v", pod.Namespace, pod.Name, err)
		return
	}
	err := client.Pods(k.client, pod.Namespace).Delete(ctx, pod.Name, &metav1.DeleteOptions{
		GracePeriodSeconds: ptr.To[int64](0), Preconditions: &metav1.Preconditions{UID: &pod.UID}})
	if err != nil && !client.IsNotFound(err) && !client.IsConflict(err) {
		log.Printf("kubelet: finish deletion of %s/%s: %v", pod.Namespace, pod.Name, err)
	}
}

func (k *Kubelet) cleanupOrphans(ctx context.Context) {
	keep := map[string]bool{}
	for _, p := range k.pods.List("", nil) {
		keep[p.UID] = true
	}
	if err := k.runtime.Cleanup(ctx, keep); err != nil {
		log.Printf("kubelet: cleanup: %v", err)
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

// heartbeat registers the Node if needed and refreshes its Ready condition.
func (k *Kubelet) heartbeat(ctx context.Context) error {
	nodes := client.Nodes(k.client)
	node, err := nodes.Get(ctx, k.nodeName)
	if client.IsNotFound(err) {
		node, err = nodes.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: k.nodeName, Labels: map[string]string{
			corev1.LabelHostname: k.nodeName, corev1.LabelOSStable: "linux", corev1.LabelArchStable: runtime.GOARCH,
		}}})
	}
	if err != nil {
		return err
	}
	now := metav1.Now()
	ready := corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady",
		Message: "kubelet is posting ready status", LastHeartbeatTime: now, LastTransitionTime: now}
	if prev := node.Status.Condition(corev1.NodeReady); prev != nil && prev.Status == corev1.ConditionTrue {
		ready.LastTransitionTime = prev.LastTransitionTime
	}
	runtimeVersion, _ := k.runtime.Version(ctx)
	node.Status = corev1.NodeStatus{
		Conditions: []corev1.NodeCondition{ready},
		Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: k.address}, {Type: corev1.NodeHostName, Address: k.nodeName}},
		NodeInfo: corev1.NodeSystemInfo{KubeletVersion: Version, ContainerRuntimeVersion: "docker://" + runtimeVersion,
			OperatingSystem: "linux", Architecture: runtime.GOARCH},
	}
	_, err = nodes.UpdateStatus(ctx, node)
	return err
}

// Handler serves the kubelet API: /healthz and
// /containerLogs/{namespace}/{pod}/{container}?follow=&tailLines=.
func (k *Kubelet) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+k.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/containerLogs/"), "/")
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/containerLogs/") || len(parts) != 3 {
			http.NotFound(w, r)
			return
		}
		pod, ok := k.pods.Get(parts[0], parts[1])
		if !ok {
			http.Error(w, "pod not found on this node", http.StatusNotFound)
			return
		}
		tail, _ := strconv.Atoi(r.URL.Query().Get("tailLines"))
		follow := r.URL.Query().Get("follow") == "true"
		w.Header().Set("Content-Type", "text/plain")
		if err := k.runtime.Logs(r.Context(), pod, parts[2], follow, tail, w); err != nil {
			log.Printf("kubelet: logs %s/%s/%s: %v", parts[0], parts[1], parts[2], err)
			http.Error(w, err.Error(), http.StatusNotFound)
		}
	})
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
