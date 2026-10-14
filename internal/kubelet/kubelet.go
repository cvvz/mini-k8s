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

	"mini-k8s/internal/api"
	"mini-k8s/internal/client"
	"mini-k8s/internal/informer"
	"mini-k8s/internal/record"
)

// Version is reported in NodeInfo.
const Version = "mini-kubelet/v2"

// Kubelet manages the Pods of one node.
type Kubelet struct {
	nodeName string
	address  string
	token    string
	client   *client.Client
	runtime  *Runtime
	recorder *record.Recorder
	pods     *informer.Informer[api.Pod, *api.Pod]

	mu      sync.Mutex
	workers map[string]chan struct{} // by Pod UID
	killing map[string]bool
}

// New returns a kubelet for nodeName reachable at address.
func New(c *client.Client, rt *Runtime, recorder *record.Recorder, nodeName, address, token string) *Kubelet {
	k := &Kubelet{nodeName: nodeName, address: address, token: token, client: c, runtime: rt, recorder: recorder,
		workers: map[string]chan struct{}{}, killing: map[string]bool{}}
	k.pods = informer.New(client.Pods(c, ""), client.ListOptions{FieldSelector: "spec.nodeName=" + nodeName}, 0)
	k.pods.AddHandler(informer.Handler[*api.Pod]{
		OnAdd:    func(p *api.Pod) { k.dispatch(p) },
		OnUpdate: func(_, p *api.Pod) { k.dispatch(p) },
		OnDelete: func(p *api.Pod) { k.dispatch(p) },
	})
	return k
}

// Run registers the node and reconciles Pods until ctx ends.
func (k *Kubelet) Run(ctx context.Context) {
	go k.heartbeatLoop(ctx)
	go k.pods.Run(ctx)
	if !informer.WaitForSync(ctx, k.pods.HasSynced) {
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
func (k *Kubelet) dispatch(p *api.Pod) {
	k.mu.Lock()
	defer k.mu.Unlock()
	ch, ok := k.workers[p.UID]
	if !ok {
		ch = make(chan struct{}, 1)
		k.workers[p.UID] = ch
		go k.worker(informer.Key(p), p.UID, ch)
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// worker serialises all work for one Pod, so a slow image pull never blocks
// other Pods.
func (k *Kubelet) worker(key, uid string, wake chan struct{}) {
	ns, name := informer.SplitKey(key)
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

func (k *Kubelet) syncPod(ctx context.Context, pod *api.Pod) {
	if pod.DeletionTimestamp != nil {
		k.terminate(ctx, pod)
		return
	}
	status := k.runtime.SyncPod(ctx, pod)
	status.HostIP = k.address
	if status.StartTime == nil {
		now := api.Now()
		status.StartTime = &now
	}
	k.recordTransitions(pod, &status)
	if equalStatus(pod.Status, status) {
		return
	}
	cp := api.DeepCopy(pod)
	cp.Status = status
	if _, err := client.Pods(k.client, pod.Namespace).UpdateStatus(ctx, cp); err != nil && !client.IsConflict(err) && !client.IsNotFound(err) {
		log.Printf("kubelet: update status of %s/%s: %v", pod.Namespace, pod.Name, err)
	}
}

func equalStatus(a, b api.PodStatus) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return bytes.Equal(aj, bj)
}

// recordTransitions emits Events for container state changes.
func (k *Kubelet) recordTransitions(pod *api.Pod, status *api.PodStatus) {
	for _, cs := range status.ContainerStatuses {
		prev := findStatus(pod.Status.ContainerStatuses, cs.Name)
		switch {
		case cs.State.Running != nil && (prev == nil || prev.State.Running == nil || prev.RestartCount != cs.RestartCount):
			k.recorder.Eventf(pod, api.EventTypeNormal, "Started", "Started container %s", cs.Name)
		case cs.State.Waiting != nil && (prev == nil || prev.State.Waiting == nil || prev.State.Waiting.Reason != cs.State.Waiting.Reason):
			switch cs.State.Waiting.Reason {
			case ReasonCrashLoopBackOff:
				k.recorder.Eventf(pod, api.EventTypeWarning, "BackOff", "Back-off restarting failed container %s", cs.Name)
			case ReasonErrImagePull, ReasonImagePullBackOff, ReasonCreateError, ReasonRunContainerError:
				k.recorder.Eventf(pod, api.EventTypeWarning, "Failed", "%s: %s", cs.State.Waiting.Reason, cs.State.Waiting.Message)
			}
		}
	}
}

// terminate stops the Pod's containers within its grace period and then
// completes the deletion in the API.
func (k *Kubelet) terminate(ctx context.Context, pod *api.Pod) {
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
			k.recorder.Eventf(pod, api.EventTypeNormal, "Killing", "Stopping container %s (grace period %ds)", c.Name, grace)
		}
	}
	if err := k.runtime.KillPod(ctx, pod, grace); err != nil {
		log.Printf("kubelet: kill %s/%s: %v", pod.Namespace, pod.Name, err)
		return
	}
	err := client.Pods(k.client, pod.Namespace).Delete(ctx, pod.Name, &api.DeleteOptions{
		GracePeriodSeconds: api.Int64(0), Preconditions: &api.Preconditions{UID: &pod.UID}})
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
		node, err = nodes.Create(ctx, &api.Node{ObjectMeta: api.ObjectMeta{Name: k.nodeName, Labels: map[string]string{
			api.LabelHostname: k.nodeName, api.LabelOS: "linux", api.LabelArch: runtime.GOARCH,
		}}})
	}
	if err != nil {
		return err
	}
	now := api.Now()
	ready := api.NodeCondition{Type: api.NodeReady, Status: api.ConditionTrue, Reason: "KubeletReady",
		Message: "kubelet is posting ready status", LastHeartbeatTime: now, LastTransitionTime: now}
	if prev := node.Status.Condition(api.NodeReady); prev != nil && prev.Status == api.ConditionTrue {
		ready.LastTransitionTime = prev.LastTransitionTime
	}
	runtimeVersion, _ := k.runtime.Version(ctx)
	node.Status = api.NodeStatus{
		Conditions: []api.NodeCondition{ready},
		Addresses:  []api.NodeAddress{{Type: api.NodeInternalIP, Address: k.address}, {Type: api.NodeHostName, Address: k.nodeName}},
		NodeInfo: api.NodeSystemInfo{KubeletVersion: Version, ContainerRuntimeVersion: "docker://" + runtimeVersion,
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
