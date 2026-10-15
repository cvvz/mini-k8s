// Package scheduler assigns unscheduled Pods to nodes with the scheduling
// framework: filter plugins drop the nodes a Pod cannot run on, score
// plugins rank the rest, and the best node is bound through the Pod's
// binding subresource.
package scheduler

import (
	"context"
	"sync"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/client/workqueue"
	"mini-k8s/pkg/scheduler/framework"
	"mini-k8s/pkg/scheduler/framework/plugins"
	"mini-k8s/pkg/util/deepcopy"
)

// Scheduler binds each pending Pod to the best node of the default profile.
type Scheduler struct {
	client    *client.Client
	pods      *informers.Informer[corev1.Pod, *corev1.Pod]
	nodes     *informers.Informer[corev1.Node, *corev1.Node]
	recorder  *record.Recorder
	queue     *workqueue.Queue
	framework *framework.Framework

	mu sync.Mutex
	// assumed holds bindings not yet visible in the Pod cache, so Pods
	// scheduled back to back see each other's resources and spreading.
	assumed map[string]*corev1.Pod
	// attempts counts failed attempts per Pod UID for the backoff.
	attempts map[string]int
}

// Backoff of unschedulable Pods, as kube-scheduler's defaults: 1s doubling
// up to 10s; cluster events retry them earlier.
const (
	initialBackoff = time.Second
	maxBackoff     = 10 * time.Second
)

// New wires the scheduler onto shared Pod and Node informers.
func New(c *client.Client, pods *informers.Informer[corev1.Pod, *corev1.Pod], nodes *informers.Informer[corev1.Node, *corev1.Node], recorder *record.Recorder) *Scheduler {
	s := &Scheduler{client: c, pods: pods, nodes: nodes, recorder: recorder, queue: workqueue.New(),
		framework: plugins.Default(), assumed: map[string]*corev1.Pod{}, attempts: map[string]int{}}
	retryPending := func() {
		for _, p := range pods.List("", nil) {
			if pending(p) {
				s.queue.Add(informers.Key(p))
			}
		}
	}
	pods.AddHandler(informers.Handler[*corev1.Pod]{
		OnAdd:    s.onPod,
		OnUpdate: func(_, p *corev1.Pod) { s.onPod(p) },
		// A Pod going away frees resources and affinity constraints.
		OnDelete: func(p *corev1.Pod) { s.forget(p.UID); retryPending() },
	})
	nodes.AddHandler(informers.Handler[*corev1.Node]{
		OnAdd:    func(*corev1.Node) { retryPending() },
		OnUpdate: func(_, _ *corev1.Node) { retryPending() },
	})
	return s
}

// Backoff is the wait after the given number of failed attempts: 1s
// doubling up to maxBackoff (the shift is bounded so it cannot overflow).
func Backoff(attempts int) time.Duration {
	return min(initialBackoff<<min(max(attempts-1, 0), 8), maxBackoff)
}

func pending(p *corev1.Pod) bool { return p.Spec.NodeName == "" && p.DeletionTimestamp == nil }

func (s *Scheduler) onPod(p *corev1.Pod) {
	if pending(p) {
		s.queue.Add(informers.Key(p))
	} else {
		s.forget(p.UID)
	}
}

func (s *Scheduler) forget(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.assumed, uid)
	delete(s.attempts, uid)
}

// Run schedules Pods one at a time until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	workqueue.Run(ctx, s.queue, "scheduler", 1, s.schedule)
}

// snapshot is the cluster with bound and assumed Pods placed.
func (s *Scheduler) snapshot() *framework.Snapshot {
	pods := s.pods.List("", nil)
	bound := map[string]bool{}
	var placed []*corev1.Pod
	for _, p := range pods {
		if p.Spec.NodeName != "" {
			bound[p.UID] = true
			placed = append(placed, p)
		}
	}
	s.mu.Lock()
	for uid, p := range s.assumed {
		if !bound[uid] {
			placed = append(placed, p)
		}
	}
	s.mu.Unlock()
	return framework.NewSnapshot(s.nodes.List("", nil), placed)
}

func (s *Scheduler) schedule(ctx context.Context, key string) error {
	ns, name := informers.SplitKey(key)
	pod, ok := s.pods.Get(ns, name)
	if !ok || !pending(pod) {
		return nil
	}
	node, reason := s.framework.Schedule(pod, s.snapshot())
	if node == "" {
		s.markUnschedulable(ctx, pod, reason)
		s.mu.Lock()
		s.attempts[pod.UID]++
		delay := Backoff(s.attempts[pod.UID])
		s.mu.Unlock()
		s.queue.AddAfter(key, delay)
		return nil
	}
	if err := s.client.BindPod(ctx, ns, name, node); err != nil {
		if client.IsConflict(err) || client.IsNotFound(err) {
			return nil
		}
		return err
	}
	assumed := deepcopy.Copy(pod)
	assumed.Spec.NodeName = node
	s.mu.Lock()
	s.assumed[pod.UID] = assumed
	delete(s.attempts, pod.UID)
	s.mu.Unlock()
	s.recorder.Eventf(pod, corev1.EventTypeNormal, "Scheduled", "Successfully assigned %s/%s to %s", ns, name, node)
	return nil
}

func (s *Scheduler) markUnschedulable(ctx context.Context, pod *corev1.Pod, reason string) {
	cp := deepcopy.Copy(pod)
	if !cp.Status.SetCondition(corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: "Unschedulable", Message: reason, LastTransitionTime: metav1.Now()}) {
		return
	}
	if _, err := client.Pods(s.client, pod.Namespace).UpdateStatus(ctx, cp); err == nil {
		s.recorder.Eventf(pod, corev1.EventTypeWarning, "FailedScheduling", "%s", reason)
	}
}
