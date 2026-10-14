package controller

import (
	"context"
	"time"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/workqueue"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/scheme"
	"mini-k8s/pkg/util/deepcopy"
)

// NamespaceController empties Terminating namespaces and then removes their
// "kubernetes" finalizer so the API server can delete them.
type NamespaceController struct {
	client     *client.Client
	namespaces *informers.Informer[corev1.Namespace, *corev1.Namespace]
	queue      *workqueue.Queue
}

// NewNamespaceController wires handlers onto the namespace informers.
func NewNamespaceController(c *client.Client, namespaces *informers.Informer[corev1.Namespace, *corev1.Namespace]) *NamespaceController {
	nc := &NamespaceController{client: c, namespaces: namespaces, queue: workqueue.New()}
	enqueue := func(ns *corev1.Namespace) {
		if ns.DeletionTimestamp != nil {
			nc.queue.Add(ns.Name)
		}
	}
	namespaces.AddHandler(informers.Handler[*corev1.Namespace]{OnAdd: enqueue, OnUpdate: func(_, ns *corev1.Namespace) { enqueue(ns) }})
	return nc
}

// Run processes namespaces until ctx ends.
func (nc *NamespaceController) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, nc.queue, "namespace", workers, nc.sync)
}

func (nc *NamespaceController) sync(ctx context.Context, name string) error {
	ns, ok := nc.namespaces.Get("", name)
	if !ok || ns.DeletionTimestamp == nil || !ns.HasFinalizer(corev1.FinalizerKubernetes) {
		return nil
	}
	remaining := 0
	for _, res := range scheme.All {
		if !res.Namespaced {
			continue
		}
		objs := client.For[metav1.PartialObjectMetadata](nc.client, res, name)
		list, err := objs.List(ctx, client.ListOptions{})
		if err != nil {
			return err
		}
		for _, obj := range list.Items {
			remaining++
			if obj.DeletionTimestamp != nil {
				continue
			}
			if err := objs.Delete(ctx, obj.Name, nil); err != nil && !client.IsNotFound(err) {
				return err
			}
		}
	}
	if remaining > 0 {
		// Pods terminate gracefully; check again shortly.
		nc.queue.AddAfter(name, time.Second)
		return nil
	}
	cp := deepcopy.Copy(ns)
	cp.RemoveFinalizer(corev1.FinalizerKubernetes)
	patch := map[string]any{"metadata": map[string]any{"resourceVersion": ns.ResourceVersion, "finalizers": cp.Finalizers}}
	_, err := client.Namespaces(nc.client).Patch(ctx, name, patch)
	if client.IsNotFound(err) || client.IsConflict(err) {
		return nil
	}
	return err
}
