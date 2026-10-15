// Package nodelifecycle implements the node lifecycle controller: it
// watches each node's Lease and, when a kubelet stops renewing it, marks
// the node's conditions Unknown and its Pods not ready; it keeps the
// not-ready and unreachable taints in line with the Ready condition (the
// taint-eviction controller acts on the NoExecute ones), like
// k8s.io/kubernetes/pkg/controller/nodelifecycle.
package nodelifecycle

import (
	"context"
	"log"
	"reflect"
	"sync"
	"time"

	coordinationv1 "mini-k8s/pkg/apis/coordination/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/util/deepcopy"
)

// Controller monitors node health.
type Controller struct {
	client   *client.Client
	nodes    *informers.Informer[corev1.Node, *corev1.Node]
	leases   *informers.Informer[coordinationv1.Lease, *coordinationv1.Lease]
	pods     *informers.Informer[corev1.Pod, *corev1.Pod]
	recorder *record.Recorder
	// Grace is how long a node may go without a heartbeat (Lease renewal
	// or status) before it is Unknown; Period is how often nodes are checked.
	Grace, Period time.Duration
	now           func() time.Time

	mu sync.Mutex
	// seen records, per node, the last heartbeat observed and when (on the
	// controller's clock), so a restarted controller gives every node a
	// full grace period, as Kubernetes' nodeHealthMap does.
	seen map[string]observation
}

type observation struct {
	heartbeat time.Time
	at        time.Time
}

// NewController wires the controller onto shared informers.
func NewController(c *client.Client, nodes *informers.Informer[corev1.Node, *corev1.Node],
	leases *informers.Informer[coordinationv1.Lease, *coordinationv1.Lease], pods *informers.Informer[corev1.Pod, *corev1.Pod],
	recorder *record.Recorder) *Controller {
	return &Controller{client: c, nodes: nodes, leases: leases, pods: pods, recorder: recorder,
		Grace: 15 * time.Second, Period: 2 * time.Second, now: time.Now, seen: map[string]observation{}}
}

// Run checks every node each Period until ctx ends. While the API (or
// etcd) is unreachable no heartbeat can arrive, so nothing is judged; after
// an outage longer than Grace every node gets a full grace period again.
func (nc *Controller) Run(ctx context.Context) {
	lastHealthy := nc.now()
	for {
		if err := nc.client.Health(ctx); err == nil {
			if nc.now().Sub(lastHealthy) > nc.Grace {
				nc.mu.Lock()
				nc.seen = map[string]observation{}
				nc.mu.Unlock()
			}
			lastHealthy = nc.now()
			for _, n := range nc.nodes.List("", nil) {
				if err := nc.monitor(ctx, n); err != nil && ctx.Err() == nil {
					log.Printf("node-lifecycle: %s: %v", n.Name, err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(nc.Period):
		}
	}
}

// heartbeat is the latest sign of life: the Lease renewal or a status
// write by the kubelet.
func (nc *Controller) heartbeat(n *corev1.Node) time.Time {
	var hb time.Time
	if c := n.Status.Condition(corev1.NodeReady); c != nil && c.Reason != reasonUnknown {
		hb = c.LastHeartbeatTime.Time
	}
	if l, ok := nc.leases.Get(corev1.NamespaceNodeLease, n.Name); ok && l.Spec.RenewTime != nil && l.Spec.RenewTime.After(hb) {
		hb = l.Spec.RenewTime.Time
	}
	return hb
}

const (
	reasonUnknown        = "NodeStatusUnknown"
	reasonNeverUpdated   = "NodeStatusNeverUpdated"
	messageStopped       = "Kubelet stopped posting node status."
	messageNeverUpdated  = "Kubelet never posted node status."
	eventReasonNotReady  = "NodeNotReady"
	eventMessageNotReady = "Node %s status is now: NodeNotReady"
)

func (nc *Controller) monitor(ctx context.Context, cached *corev1.Node) error {
	now := nc.now()
	hb := nc.heartbeat(cached)
	nc.mu.Lock()
	obs, ok := nc.seen[cached.Name]
	if !ok || !hb.Equal(obs.heartbeat) {
		obs = observation{heartbeat: hb, at: now}
		nc.seen[cached.Name] = obs
	}
	nc.mu.Unlock()

	node := deepcopy.Copy(cached)
	ready := node.Status.Condition(corev1.NodeReady)
	if now.Sub(obs.at) > nc.Grace && (ready == nil || ready.Status != corev1.ConditionUnknown) {
		markUnknown(node, metav1.NewTime(now))
		updated, err := client.Nodes(nc.client).UpdateStatus(ctx, node)
		if err != nil {
			return err
		}
		node = updated
		nc.recorder.Eventf(node, corev1.EventTypeNormal, eventReasonNotReady, eventMessageNotReady, node.Name)
	}
	ready = node.Status.Condition(corev1.NodeReady)
	if ready != nil && ready.Status == corev1.ConditionUnknown {
		// The kubelet is gone: nobody else will report its Pods.
		nc.markPodsNotReady(ctx, node.Name)
	}
	if taints, changed := conditionTaints(node.Spec.Taints, ready, metav1.NewTime(now)); changed {
		node.Spec.Taints = taints
		if _, err := client.Nodes(nc.client).Update(ctx, node); err != nil && !client.IsConflict(err) {
			return err
		}
	}
	return nil
}

// markUnknown sets every condition Unknown, adding a Ready condition if
// the kubelet never posted one.
func markUnknown(node *corev1.Node, now metav1.Time) {
	if node.Status.Condition(corev1.NodeReady) == nil {
		node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeReady,
			Status: corev1.ConditionUnknown, Reason: reasonNeverUpdated, Message: messageNeverUpdated, LastTransitionTime: now})
		return
	}
	for i := range node.Status.Conditions {
		c := &node.Status.Conditions[i]
		if c.Status != corev1.ConditionUnknown {
			c.Status, c.Reason, c.Message, c.LastTransitionTime = corev1.ConditionUnknown, reasonUnknown, messageStopped, now
		}
	}
}

// conditionTaints returns the node's taints with the not-ready and
// unreachable taints matching its Ready condition (Kubernetes' taint
// nodes by condition and NoExecute taints); NoExecute ones carry the time
// they were added.
func conditionTaints(taints []corev1.Taint, ready *corev1.NodeCondition, now metav1.Time) ([]corev1.Taint, bool) {
	var want string
	switch {
	case ready == nil:
		return taints, false
	case ready.Status == corev1.ConditionUnknown:
		want = corev1.TaintNodeUnreachable
	case ready.Status == corev1.ConditionFalse:
		want = corev1.TaintNodeNotReady
	}
	var out []corev1.Taint
	have := map[string]bool{}
	for _, t := range taints {
		managed := t.Key == corev1.TaintNodeUnreachable || t.Key == corev1.TaintNodeNotReady
		if managed && t.Key != want {
			continue
		}
		if managed {
			have[t.Effect] = true
		}
		out = append(out, t)
	}
	if want != "" {
		if !have[corev1.TaintEffectNoSchedule] {
			out = append(out, corev1.Taint{Key: want, Effect: corev1.TaintEffectNoSchedule})
		}
		if !have[corev1.TaintEffectNoExecute] {
			out = append(out, corev1.Taint{Key: want, Effect: corev1.TaintEffectNoExecute, TimeAdded: &now})
		}
	}
	return out, !reflect.DeepEqual(out, taints) && !(len(out) == 0 && len(taints) == 0)
}

// markPodsNotReady sets the Ready condition of the node's Pods to False,
// so Endpoints stop routing to them (MarkPodsNotReady).
func (nc *Controller) markPodsNotReady(ctx context.Context, node string) {
	for _, p := range nc.pods.List("", nil) {
		if p.Spec.NodeName != node || !corev1.IsPodReady(p) {
			continue
		}
		cp := deepcopy.Copy(p)
		cp.Status.SetCondition(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.Now()})
		if _, err := client.Pods(nc.client, p.Namespace).UpdateStatus(ctx, cp); err != nil && !client.IsNotFound(err) && !client.IsConflict(err) {
			log.Printf("node-lifecycle: mark %s/%s not ready: %v", p.Namespace, p.Name, err)
		}
	}
}
