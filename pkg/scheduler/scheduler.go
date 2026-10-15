// Package scheduler assigns unscheduled Pods to nodes.
package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/client/workqueue"
	"mini-k8s/pkg/util/deepcopy"
)

// Scheduler binds each pending Pod to the Ready, schedulable node that runs
// the fewest Pods (ties broken by name).
type Scheduler struct {
	client   *client.Client
	pods     *informers.Informer[corev1.Pod, *corev1.Pod]
	nodes    *informers.Informer[corev1.Node, *corev1.Node]
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
func New(c *client.Client, pods *informers.Informer[corev1.Pod, *corev1.Pod], nodes *informers.Informer[corev1.Node, *corev1.Node], recorder *record.Recorder) *Scheduler {
	s := &Scheduler{client: c, pods: pods, nodes: nodes, recorder: recorder, queue: workqueue.New(), assumed: map[string]string{}}
	pods.AddHandler(informers.Handler[*corev1.Pod]{
		OnAdd:    s.onPod,
		OnUpdate: func(_, p *corev1.Pod) { s.onPod(p) },
		OnDelete: func(p *corev1.Pod) { s.forget(p.UID) },
	})
	requeuePending := func(*corev1.Node) {
		for _, p := range pods.List("", nil) {
			if pending(p) {
				s.queue.Add(informers.Key(p))
			}
		}
	}
	nodes.AddHandler(informers.Handler[*corev1.Node]{OnAdd: requeuePending, OnUpdate: func(old, cur *corev1.Node) {
		if corev1.IsNodeReady(old) != corev1.IsNodeReady(cur) || old.Spec.Unschedulable != cur.Spec.Unschedulable {
			requeuePending(cur)
		}
	}})
	return s
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
}

// Run schedules Pods one at a time until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	workqueue.Run(ctx, s.queue, "scheduler", 1, s.schedule)
}

func (s *Scheduler) schedule(ctx context.Context, key string) error {
	ns, name := informers.SplitKey(key)
	pod, ok := s.pods.Get(ns, name)
	if !ok || !pending(pod) {
		return nil
	}
	node, reason := s.pickNode(pod, time.Now())
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
	s.recorder.Eventf(pod, corev1.EventTypeNormal, "Scheduled", "Successfully assigned %s/%s to %s", ns, name, node)
	return nil
}

func (s *Scheduler) pickNode(pod *corev1.Pod, now time.Time) (string, string) {
	load := map[string]int{}
	bound := map[string]bool{}
	for _, p := range s.pods.List("", nil) {
		if p.Spec.NodeName != "" {
			bound[p.UID] = true
			if !corev1.IsPodTerminal(p) {
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
	best := ""
	rejected := map[string]int{} // reason -> nodes
	for _, n := range nodes {
		if taint, ok := corev1.UntoleratedTaint(n.Spec.Taints, pod.Spec.Tolerations); ok {
			rejected["had untolerated taint "+taint.String()]++
			continue
		}
		switch {
		case !corev1.IsNodeReady(n):
			rejected["were not ready"]++
		case n.Spec.Unschedulable:
			rejected["were unschedulable"]++
		case best == "" || load[n.Name] < load[best]:
			best = n.Name
		}
	}
	if best != "" {
		return best, ""
	}
	return "", unschedulableMessage(len(nodes), rejected)
}

// unschedulableMessage explains why no node fits, like the Kubernetes
// scheduler: "0/3 nodes are available: 1 node(s) had untolerated taint
// {node-role.kubernetes.io/control-plane: }, 2 node(s) were unschedulable."
func unschedulableMessage(total int, rejected map[string]int) string {
	reasons := make([]string, 0, len(rejected))
	for reason, n := range rejected {
		reasons = append(reasons, fmt.Sprintf("%d node(s) %s", n, reason))
	}
	sort.Strings(reasons)
	if len(reasons) == 0 {
		return fmt.Sprintf("0/%d nodes are available.", total)
	}
	return fmt.Sprintf("0/%d nodes are available: %s.", total, strings.Join(reasons, ", "))
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
