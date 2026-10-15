// Package podgc implements the Pod garbage collector's orphan sweep: Pods
// bound to a node that no longer exists are marked Failed and deleted, like
// gcOrphaned in k8s.io/kubernetes/pkg/controller/podgc. Without it, Pods of
// a node removed while the cluster was down would stay Running forever and
// their controllers would never replace them.
package podgc

import (
	"context"
	"log"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/util/deepcopy"
)

// Kubernetes' gcCheckPeriod and quarantineTime.
const (
	DefaultPeriod     = 20 * time.Second
	DefaultQuarantine = 40 * time.Second
)

// Controller deletes the Pods of vanished nodes.
type Controller struct {
	client *client.Client
	pods   *informers.Informer[corev1.Pod, *corev1.Pod]
	nodes  *informers.Informer[corev1.Node, *corev1.Node]
	// Period is how often Pods are checked; Quarantine is how long a node
	// must stay absent before its Pods go, so a node that is being
	// re-registered or not yet seen by the informer keeps them.
	Period, Quarantine time.Duration
	now                func() time.Time
	// missing records when each absent node was first found referenced.
	missing map[string]time.Time
}

// NewController wires the collector onto shared informers.
func NewController(c *client.Client, pods *informers.Informer[corev1.Pod, *corev1.Pod],
	nodes *informers.Informer[corev1.Node, *corev1.Node]) *Controller {
	return &Controller{client: c, pods: pods, nodes: nodes, Period: DefaultPeriod, Quarantine: DefaultQuarantine,
		now: time.Now, missing: map[string]time.Time{}}
}

// Run sweeps every Period until ctx ends.
func (gc *Controller) Run(ctx context.Context) {
	ticker := time.NewTicker(gc.Period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			gc.gcOrphaned(ctx)
		}
	}
}

// gcOrphaned deletes the Pods bound to nodes that have been absent for the
// quarantine and that the API confirms are gone.
func (gc *Controller) gcOrphaned(ctx context.Context) {
	now := gc.now()
	orphans := map[string][]*corev1.Pod{}
	for _, p := range gc.pods.List("", nil) {
		if p.Spec.NodeName == "" {
			continue
		}
		if _, ok := gc.nodes.Get("", p.Spec.NodeName); !ok {
			orphans[p.Spec.NodeName] = append(orphans[p.Spec.NodeName], p)
		}
	}
	for name := range gc.missing {
		if _, ok := orphans[name]; !ok {
			delete(gc.missing, name)
		}
	}
	for name, pods := range orphans {
		first, ok := gc.missing[name]
		if !ok {
			gc.missing[name] = now
			continue
		}
		if now.Sub(first) < gc.Quarantine {
			continue
		}
		// The informer may lag behind a node that was just created.
		if _, err := client.Nodes(gc.client).Get(ctx, name); !client.IsNotFound(err) {
			if err != nil {
				log.Printf("pod-gc: check node %s: %v", name, err)
			} else {
				delete(gc.missing, name)
			}
			continue
		}
		for _, p := range pods {
			log.Printf("pod-gc: deleting Pod %s/%s of vanished node %s", p.Namespace, p.Name, name)
			if err := gc.markFailedAndDelete(ctx, p); err != nil {
				log.Printf("pod-gc: delete %s/%s: %v", p.Namespace, p.Name, err)
			}
		}
	}
}

// markFailedAndDelete records why the Pod goes, then deletes it at once:
// no kubelet is left to stop its containers.
func (gc *Controller) markFailedAndDelete(ctx context.Context, p *corev1.Pod) error {
	pods := client.Pods(gc.client, p.Namespace)
	if !corev1.IsPodTerminal(p) {
		cp := deepcopy.Copy(p)
		cp.Status.Phase = corev1.PodFailed
		cp.Status.SetCondition(corev1.PodCondition{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue,
			Reason: "DeletionByPodGC", Message: "PodGC: node no longer exists", LastTransitionTime: metav1.Now()})
		if _, err := pods.UpdateStatus(ctx, cp); err != nil {
			if client.IsNotFound(err) || client.IsConflict(err) {
				return nil // gone, or changed: the next sweep sees the new version
			}
			return err
		}
	}
	zero := int64(0)
	err := pods.Delete(ctx, p.Name, &metav1.DeleteOptions{GracePeriodSeconds: &zero, Preconditions: &metav1.Preconditions{UID: &p.UID}})
	if client.IsNotFound(err) || client.IsConflict(err) {
		return nil
	}
	return err
}
