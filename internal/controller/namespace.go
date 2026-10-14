package controller

import (
	"context"
	"time"

	"mini-k8s/internal/api"
	"mini-k8s/internal/client"
	"mini-k8s/internal/informer"
	"mini-k8s/internal/workqueue"
)

// NamespaceController empties Terminating namespaces and then removes their
// "kubernetes" finalizer so the API server can delete them.
type NamespaceController struct {
	client     *client.Client
	namespaces *informer.Informer[api.Namespace, *api.Namespace]
	queue      *workqueue.Queue
}

// NewNamespaceController wires handlers onto the namespace informer.
func NewNamespaceController(c *client.Client, namespaces *informer.Informer[api.Namespace, *api.Namespace]) *NamespaceController {
	nc := &NamespaceController{client: c, namespaces: namespaces, queue: workqueue.New()}
	enqueue := func(ns *api.Namespace) {
		if ns.DeletionTimestamp != nil {
			nc.queue.Add(ns.Name)
		}
	}
	namespaces.AddHandler(informer.Handler[*api.Namespace]{OnAdd: enqueue, OnUpdate: func(_, ns *api.Namespace) { enqueue(ns) }})
	return nc
}

// Run processes namespaces until ctx ends.
func (nc *NamespaceController) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, nc.queue, "namespace", workers, nc.sync)
}

func (nc *NamespaceController) sync(ctx context.Context, name string) error {
	ns, ok := nc.namespaces.Get("", name)
	if !ok || ns.DeletionTimestamp == nil || !ns.HasFinalizer(api.FinalizerNamespace) {
		return nil
	}
	remaining := 0
	for _, res := range api.All {
		if !res.Namespaced {
			continue
		}
		objs := client.For[api.PartialObjectMetadata](nc.client, res, name)
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
	cp := api.DeepCopy(ns)
	cp.RemoveFinalizer(api.FinalizerNamespace)
	patch := map[string]any{"metadata": map[string]any{"resourceVersion": ns.ResourceVersion, "finalizers": cp.Finalizers}}
	_, err := client.Namespaces(nc.client).Patch(ctx, name, patch)
	if client.IsNotFound(err) || client.IsConflict(err) {
		return nil
	}
	return err
}
