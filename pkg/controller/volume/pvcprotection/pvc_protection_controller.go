// Package pvcprotection removes the kubernetes.io/pvc-protection finalizer
// from deleted claims once no Pod uses them, like
// k8s.io/kubernetes/pkg/controller/volume/pvcprotection: a claim in use
// stays Terminating until its last Pod is gone.
package pvcprotection

import (
	"context"
	"slices"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/workqueue"
	"mini-k8s/pkg/util/deepcopy"
)

// Controller releases claims that are deleted and unused.
type Controller struct {
	client *client.Client
	claims *informers.Informer[corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim]
	pods   *informers.Informer[corev1.Pod, *corev1.Pod]
	queue  *workqueue.Queue
}

// NewController wires the controller onto shared informers.
func NewController(c *client.Client, claims *informers.Informer[corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim],
	pods *informers.Informer[corev1.Pod, *corev1.Pod]) *Controller {
	pc := &Controller{client: c, claims: claims, pods: pods, queue: workqueue.New()}
	onClaim := func(pvc *corev1.PersistentVolumeClaim) {
		if pvc.DeletionTimestamp != nil {
			pc.queue.Add(informers.Key(pvc))
		}
	}
	claims.AddHandler(informers.Handler[*corev1.PersistentVolumeClaim]{
		OnAdd: onClaim, OnUpdate: func(_, pvc *corev1.PersistentVolumeClaim) { onClaim(pvc) },
	})
	onPod := func(p *corev1.Pod) {
		for _, name := range ClaimsOf(p) {
			pc.queue.Add(p.Namespace + "/" + name)
		}
	}
	pods.AddHandler(informers.Handler[*corev1.Pod]{
		OnUpdate: func(_, p *corev1.Pod) { onPod(p) }, OnDelete: onPod,
	})
	return pc
}

// ClaimsOf lists the claims a Pod mounts.
func ClaimsOf(p *corev1.Pod) []string {
	var names []string
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			names = append(names, v.PersistentVolumeClaim.ClaimName)
		}
	}
	return names
}

// Run processes claims until ctx ends.
func (pc *Controller) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, pc.queue, "pvc-protection", workers, pc.sync)
}

func (pc *Controller) sync(ctx context.Context, key string) error {
	ns, name := informers.SplitKey(key)
	pvc, ok := pc.claims.Get(ns, name)
	if !ok || pvc.DeletionTimestamp == nil || !pvc.HasFinalizer(corev1.PVCProtectionFinalizer) || pc.inUse(pvc) {
		return nil
	}
	cp := deepcopy.Copy(pvc)
	cp.Finalizers = slices.DeleteFunc(cp.Finalizers, func(f string) bool { return f == corev1.PVCProtectionFinalizer })
	_, err := client.PersistentVolumeClaims(pc.client, ns).Update(ctx, cp)
	if client.IsNotFound(err) || client.IsConflict(err) {
		return nil
	}
	return err
}

// inUse reports whether a Pod that has not finished mounts the claim.
func (pc *Controller) inUse(pvc *corev1.PersistentVolumeClaim) bool {
	for _, p := range pc.pods.List(pvc.Namespace, nil) {
		if !corev1.IsPodTerminal(p) && slices.Contains(ClaimsOf(p), pvc.Name) {
			return true
		}
	}
	return false
}
