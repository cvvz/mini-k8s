// Package tainteviction implements the taint-eviction controller: Pods on
// a node with NoExecute taints they do not tolerate are deleted at once,
// and Pods that tolerate them for tolerationSeconds are deleted when that
// time has passed since the taint was added, like
// k8s.io/kubernetes/pkg/controller/tainteviction.
package tainteviction

import (
	"context"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/client/workqueue"
)

// Controller evicts Pods from NoExecute-tainted nodes.
type Controller struct {
	client   *client.Client
	nodes    *informers.Informer[corev1.Node, *corev1.Node]
	pods     *informers.Informer[corev1.Pod, *corev1.Pod]
	recorder *record.Recorder
	queue    *workqueue.Queue
	now      func() time.Time
	started  time.Time
}

// NewController wires the controller onto shared informers.
func NewController(c *client.Client, nodes *informers.Informer[corev1.Node, *corev1.Node],
	pods *informers.Informer[corev1.Pod, *corev1.Pod], recorder *record.Recorder) *Controller {
	tc := &Controller{client: c, nodes: nodes, pods: pods, recorder: recorder, queue: workqueue.New(), now: time.Now, started: time.Now()}
	enqueuePod := func(p *corev1.Pod) {
		if p.Spec.NodeName != "" {
			tc.queue.Add(informers.Key(p))
		}
	}
	pods.AddHandler(informers.Handler[*corev1.Pod]{OnAdd: enqueuePod, OnUpdate: func(_, p *corev1.Pod) { enqueuePod(p) }})
	enqueueNode := func(n *corev1.Node) {
		if len(noExecute(n.Spec.Taints)) == 0 {
			return
		}
		for _, p := range pods.List("", nil) {
			if p.Spec.NodeName == n.Name {
				tc.queue.Add(informers.Key(p))
			}
		}
	}
	nodes.AddHandler(informers.Handler[*corev1.Node]{OnAdd: enqueueNode, OnUpdate: func(_, n *corev1.Node) { enqueueNode(n) }})
	return tc
}

// Run processes Pods until ctx ends.
func (tc *Controller) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, tc.queue, "taint-eviction", workers, tc.sync)
}

func noExecute(taints []corev1.Taint) []corev1.Taint {
	var out []corev1.Taint
	for _, t := range taints {
		if t.Effect == corev1.TaintEffectNoExecute {
			out = append(out, t)
		}
	}
	return out
}

// EvictionTime returns when a Pod with tolerations must leave a node with
// taints, like Kubernetes: a NoExecute taint without a matching toleration
// means now; a taint tolerated (by the first matching toleration) for
// tolerationSeconds expires that long after it was added, but no earlier
// than notBefore (when the controller started watching, since Kubernetes
// counts from when it observes a taint). ok is false when the Pod may stay
// for good.
func EvictionTime(taints []corev1.Taint, tolerations []corev1.Toleration, now, notBefore time.Time) (time.Time, bool) {
	var deadline time.Time
	found := false
	for _, taint := range noExecute(taints) {
		var used *corev1.Toleration
		for i := range tolerations {
			if tolerations[i].ToleratesTaint(taint) {
				used = &tolerations[i]
				break
			}
		}
		if used == nil {
			return now, true
		}
		if used.TolerationSeconds == nil {
			continue // tolerated for good
		}
		start := now
		if taint.TimeAdded != nil {
			start = taint.TimeAdded.Time
		}
		if start.Before(notBefore) {
			start = notBefore
		}
		if until := start.Add(time.Duration(max(*used.TolerationSeconds, 0)) * time.Second); !found || until.Before(deadline) {
			deadline, found = until, true
		}
	}
	return deadline, found
}

func (tc *Controller) sync(ctx context.Context, key string) error {
	ns, name := informers.SplitKey(key)
	pod, ok := tc.pods.Get(ns, name)
	if !ok || pod.Spec.NodeName == "" || pod.DeletionTimestamp != nil || corev1.IsPodTerminal(pod) {
		return nil
	}
	node, ok := tc.nodes.Get("", pod.Spec.NodeName)
	if !ok {
		return nil
	}
	now := tc.now()
	// Pods created before default tolerations existed get them here too.
	spec := corev1.PodSpec{Tolerations: append([]corev1.Toleration(nil), pod.Spec.Tolerations...)}
	corev1.AddDefaultTolerations(&spec)
	deadline, evict := EvictionTime(node.Spec.Taints, spec.Tolerations, now, tc.started)
	if !evict {
		return nil
	}
	if wait := deadline.Sub(now); wait > 0 {
		tc.queue.AddAfter(key, wait)
		return nil
	}
	tc.recorder.Eventf(pod, corev1.EventTypeNormal, "TaintManagerEviction", "Marking for deletion Pod %s/%s", ns, name)
	err := client.Pods(tc.client, ns).Delete(ctx, name, &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}})
	if client.IsNotFound(err) || client.IsConflict(err) {
		return nil
	}
	return err
}
