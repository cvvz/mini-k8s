// Package pvprotection removes the kubernetes.io/pv-protection finalizer
// from deleted volumes once they are no longer bound, like
// k8s.io/kubernetes/pkg/controller/volume/pvprotection.
package pvprotection

import (
	"context"
	"slices"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/workqueue"
	"mini-k8s/pkg/util/deepcopy"
)

// Controller releases volumes that are deleted and unbound.
type Controller struct {
	client  *client.Client
	volumes *informers.Informer[corev1.PersistentVolume, *corev1.PersistentVolume]
	queue   *workqueue.Queue
}

// NewController wires the controller onto a shared informer.
func NewController(c *client.Client, volumes *informers.Informer[corev1.PersistentVolume, *corev1.PersistentVolume]) *Controller {
	pc := &Controller{client: c, volumes: volumes, queue: workqueue.New()}
	on := func(pv *corev1.PersistentVolume) {
		if pv.DeletionTimestamp != nil {
			pc.queue.Add(pv.Name)
		}
	}
	volumes.AddHandler(informers.Handler[*corev1.PersistentVolume]{
		OnAdd: on, OnUpdate: func(_, pv *corev1.PersistentVolume) { on(pv) },
	})
	return pc
}

// Run processes volumes until ctx ends.
func (pc *Controller) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, pc.queue, "pv-protection", workers, pc.sync)
}

func (pc *Controller) sync(ctx context.Context, name string) error {
	pv, ok := pc.volumes.Get("", name)
	if !ok || pv.DeletionTimestamp == nil || !pv.HasFinalizer(corev1.PVProtectionFinalizer) || pv.Status.Phase == corev1.VolumeBound {
		return nil
	}
	cp := deepcopy.Copy(pv)
	cp.Finalizers = slices.DeleteFunc(cp.Finalizers, func(f string) bool { return f == corev1.PVProtectionFinalizer })
	_, err := client.PersistentVolumes(pc.client).Update(ctx, cp)
	if client.IsNotFound(err) || client.IsConflict(err) {
		return nil
	}
	return err
}
