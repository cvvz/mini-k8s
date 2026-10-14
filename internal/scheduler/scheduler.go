// Package scheduler assigns unscheduled Pods to nodes.
package scheduler

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"mini-k8s/internal/api"
	"mini-k8s/internal/client"
	"mini-k8s/internal/informer"
	"mini-k8s/internal/record"
	"mini-k8s/internal/workqueue"
)

// Scheduler binds each pending Pod to the Ready, schedulable node that runs
// the fewest Pods (ties broken by name).
type Scheduler struct {
	client   *client.Client
	pods     *informer.Informer[api.Pod, *api.Pod]
	nodes    *informer.Informer[api.Node, *api.Node]
	recorder *record.Recorder
	queue    *workqueue.Queue

	mu sync.Mutex
	// assumed holds bindings not yet visible in the Pod cache, so Pods
	// scheduled back to back still spread across nodes.
	assumed map[string]string
}

// retryInterval is how often unschedulable Pods are retried.
const retryInterval = 2 * time.Second

// New wires the scheduler onto shared Pod and Node informers.
func New(c *client.Client, pods *informer.Informer[api.Pod, *api.Pod], nodes *informer.Informer[api.Node, *api.Node], recorder *record.Recorder) *Scheduler {
	s := &Scheduler{client: c, pods: pods, nodes: nodes, recorder: recorder, queue: workqueue.New(), assumed: map[string]string{}}
	pods.AddHandler(informer.Handler[*api.Pod]{
		OnAdd:    s.onPod,
		OnUpdate: func(_, p *api.Pod) { s.onPod(p) },
		OnDelete: func(p *api.Pod) { s.forget(p.UID) },
	})
	requeuePending := func(*api.Node) {
		for _, p := range pods.List("", nil) {
			if pending(p) {
				s.queue.Add(informer.Key(p))
			}
		}
	}
	nodes.AddHandler(informer.Handler[*api.Node]{OnAdd: requeuePending, OnUpdate: func(old, cur *api.Node) {
		if api.IsNodeReady(old, time.Now()) != api.IsNodeReady(cur, time.Now()) || old.Spec.Unschedulable != cur.Spec.Unschedulable {
			requeuePending(cur)
		}
	}})
	return s
}

func pending(p *api.Pod) bool { return p.Spec.NodeName == "" && p.DeletionTimestamp == nil }

func (s *Scheduler) onPod(p *api.Pod) {
	if pending(p) {
		s.queue.Add(informer.Key(p))
	} else {
		s.forget(p.UID)
	}
}

func (s *Scheduler) forget(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.assumed, uid)
}

// Run schedules Pods one at a time until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	workqueue.Run(ctx, s.queue, "scheduler", 1, s.schedule)
}

func (s *Scheduler) schedule(ctx context.Context, key string) error {
	ns, name := informer.SplitKey(key)
	pod, ok := s.pods.Get(ns, name)
	if !ok || !pending(pod) {
		return nil
	}
	node, reason := s.pickNode(time.Now())
	if node == "" {
		s.markUnschedulable(ctx, pod, reason)
		s.queue.AddAfter(key, retryInterval)
		return nil
	}
	if err := s.client.BindPod(ctx, ns, name, node); err != nil {
		if client.IsConflict(err) || client.IsNotFound(err) {
			return nil
		}
		return err
	}
	s.mu.Lock()
	s.assumed[pod.UID] = node
	s.mu.Unlock()
	s.recorder.Eventf(pod, api.EventTypeNormal, "Scheduled", "Successfully assigned %s/%s to %s", ns, name, node)
	return nil
}

func (s *Scheduler) pickNode(now time.Time) (string, string) {
	load := map[string]int{}
	bound := map[string]bool{}
	for _, p := range s.pods.List("", nil) {
		if p.Spec.NodeName != "" {
			bound[p.UID] = true
			if !api.IsPodTerminal(p) {
				load[p.Spec.NodeName]++
			}
		}
	}
	s.mu.Lock()
	for uid, node := range s.assumed {
		if !bound[uid] {
			load[node]++
		}
	}
	s.mu.Unlock()
	nodes := s.nodes.List("", nil)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	best, notReady, cordoned := "", 0, 0
	for _, n := range nodes {
		switch {
		case !api.IsNodeReady(n, now):
			notReady++
		case n.Spec.Unschedulable:
			cordoned++
		case best == "" || load[n.Name] < load[best]:
			best = n.Name
		}
	}
	if best != "" {
		return best, ""
	}
	return "", fmt.Sprintf("0/%d nodes are available: %d node(s) were not ready, %d node(s) were unschedulable", len(nodes), notReady, cordoned)
}

func (s *Scheduler) markUnschedulable(ctx context.Context, pod *api.Pod, reason string) {
	cp := api.DeepCopy(pod)
	if !cp.Status.SetCondition(api.PodCondition{Type: api.PodScheduled, Status: api.ConditionFalse,
		Reason: "Unschedulable", Message: reason, LastTransitionTime: api.Now()}) {
		return
	}
	if _, err := client.Pods(s.client, pod.Namespace).UpdateStatus(ctx, cp); err == nil {
		s.recorder.Eventf(pod, api.EventTypeWarning, "FailedScheduling", "%s", reason)
	}
}
