package controller

import (
	"context"
	"log"
	"time"

	"mini-k8s/internal/api"
	"mini-k8s/internal/client"
	"mini-k8s/internal/informer"
)

type metaInformer = informer.Informer[api.PartialObjectMetadata, *api.PartialObjectMetadata]

// GarbageCollector implements ownerReference semantics:
//   - Background: a dependent whose owners are all gone is deleted.
//   - Foreground: an owner with the foregroundDeletion finalizer stays until
//     its dependents are gone.
//   - Orphan: an owner with the orphan finalizer first has its references
//     removed from every dependent.
type GarbageCollector struct {
	client  *client.Client
	sources map[string]*metaInformer // by resource
	trigger chan struct{}
}

// GCResources are the resources the garbage collector tracks: every kind
// that can own or be owned. Events are excluded; they never take part.
func GCResources() []api.ResourceInfo {
	var out []api.ResourceInfo
	for _, r := range api.All {
		if r.Resource != api.Events.Resource {
			out = append(out, r)
		}
	}
	return out
}

type gcEntry struct {
	res api.ResourceInfo
	obj *api.PartialObjectMetadata
}

// NewGarbageCollector watches metadata of the given resources.
func NewGarbageCollector(c *client.Client, resources []api.ResourceInfo) *GarbageCollector {
	gc := &GarbageCollector{client: c, sources: map[string]*metaInformer{}, trigger: make(chan struct{}, 1)}
	poke := func() {
		select {
		case gc.trigger <- struct{}{}:
		default:
		}
	}
	for _, res := range resources {
		inf := informer.New(client.For[api.PartialObjectMetadata](c, res, ""), client.ListOptions{}, 0)
		inf.AddHandler(informer.Handler[*api.PartialObjectMetadata]{
			OnAdd:    func(*api.PartialObjectMetadata) { poke() },
			OnUpdate: func(old, cur *api.PartialObjectMetadata) { poke() },
			OnDelete: func(*api.PartialObjectMetadata) { poke() },
		})
		gc.sources[res.Resource] = inf
	}
	return gc
}

// Run collects garbage whenever watched objects change, and every 10s.
func (gc *GarbageCollector) Run(ctx context.Context) {
	synced := make([]func() bool, 0, len(gc.sources))
	for _, inf := range gc.sources {
		go inf.Run(ctx)
		synced = append(synced, inf.HasSynced)
	}
	if !informer.WaitForSync(ctx, synced...) {
		return
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		gc.collect(ctx)
		select {
		case <-ctx.Done():
			return
		case <-gc.trigger:
			// Let bursts of related changes settle into one pass.
			time.Sleep(200 * time.Millisecond)
		case <-ticker.C:
		}
	}
}

func (gc *GarbageCollector) collect(ctx context.Context) {
	byUID := map[string]gcEntry{}
	dependents := map[string][]gcEntry{}
	for resource, inf := range gc.sources {
		res, _ := api.Lookup(resource)
		for _, obj := range inf.List("", nil) {
			e := gcEntry{res: res, obj: obj}
			byUID[obj.UID] = e
			for _, ref := range obj.OwnerReferences {
				dependents[ref.UID] = append(dependents[ref.UID], e)
			}
		}
	}
	for uid, owner := range byUID {
		if owner.obj.DeletionTimestamp == nil {
			continue
		}
		switch {
		case owner.obj.HasFinalizer(api.FinalizerOrphan):
			gc.orphan(ctx, owner, dependents[uid])
		case owner.obj.HasFinalizer(api.FinalizerForeground):
			gc.foreground(ctx, owner, dependents[uid])
		}
	}
	for _, dep := range byUID {
		if dep.obj.DeletionTimestamp != nil || len(dep.obj.OwnerReferences) == 0 {
			continue
		}
		gc.deleteIfOrphaned(ctx, dep, byUID)
	}
}

func (gc *GarbageCollector) resource(e gcEntry) client.Resource[api.PartialObjectMetadata, *api.PartialObjectMetadata] {
	return client.For[api.PartialObjectMetadata](gc.client, e.res, e.obj.Namespace)
}

// orphan strips references to owner from its dependents, then lets the
// owner go by removing the orphan finalizer.
func (gc *GarbageCollector) orphan(ctx context.Context, owner gcEntry, deps []gcEntry) {
	for _, dep := range deps {
		var kept []api.OwnerReference
		for _, ref := range dep.obj.OwnerReferences {
			if ref.UID != owner.obj.UID {
				kept = append(kept, ref)
			}
		}
		patch := map[string]any{"metadata": map[string]any{"resourceVersion": dep.obj.ResourceVersion, "ownerReferences": kept}}
		if _, err := gc.resource(dep).Patch(ctx, dep.obj.Name, patch); err != nil && !client.IsNotFound(err) {
			log.Printf("gc: orphan %s/%s: %v", dep.res.Resource, dep.obj.Name, err)
			return
		}
	}
	gc.removeFinalizer(ctx, owner, api.FinalizerOrphan)
}

// foreground deletes dependents and releases the owner once none remain.
func (gc *GarbageCollector) foreground(ctx context.Context, owner gcEntry, deps []gcEntry) {
	if len(deps) == 0 {
		gc.removeFinalizer(ctx, owner, api.FinalizerForeground)
		return
	}
	policy := api.DeletePropagationForeground
	for _, dep := range deps {
		if dep.obj.DeletionTimestamp != nil {
			continue
		}
		err := gc.resource(dep).Delete(ctx, dep.obj.Name, &api.DeleteOptions{
			PropagationPolicy: &policy, Preconditions: &api.Preconditions{UID: &dep.obj.UID}})
		if err != nil && !client.IsNotFound(err) && !client.IsConflict(err) {
			log.Printf("gc: delete dependent %s/%s: %v", dep.res.Resource, dep.obj.Name, err)
		}
	}
}

func (gc *GarbageCollector) removeFinalizer(ctx context.Context, e gcEntry, finalizer string) {
	cp := api.DeepCopy(e.obj)
	cp.RemoveFinalizer(finalizer)
	patch := map[string]any{"metadata": map[string]any{"resourceVersion": e.obj.ResourceVersion, "finalizers": cp.Finalizers}}
	if _, err := gc.resource(e).Patch(ctx, e.obj.Name, patch); err != nil && !client.IsNotFound(err) && !client.IsConflict(err) {
		log.Printf("gc: remove finalizer %s from %s/%s: %v", finalizer, e.res.Resource, e.obj.Name, err)
	}
}

// deleteIfOrphaned deletes dep when none of its owners exist any more. The
// cache may lag, so absence is confirmed against the API before deleting.
func (gc *GarbageCollector) deleteIfOrphaned(ctx context.Context, dep gcEntry, byUID map[string]gcEntry) {
	for _, ref := range dep.obj.OwnerReferences {
		if _, ok := byUID[ref.UID]; ok {
			return
		}
		res, ok := api.ForKind(ref.APIVersion, ref.Kind)
		if !ok {
			return // unknown owner kinds are never assumed gone
		}
		ns := ""
		if res.Namespaced {
			ns = dep.obj.Namespace
		}
		owner, err := client.For[api.PartialObjectMetadata](gc.client, res, ns).Get(ctx, ref.Name)
		if err == nil && owner.UID == ref.UID {
			return
		}
		if err != nil && !client.IsNotFound(err) {
			return
		}
	}
	policy := api.DeletePropagationBackground
	err := gc.resource(dep).Delete(ctx, dep.obj.Name, &api.DeleteOptions{
		PropagationPolicy: &policy, Preconditions: &api.Preconditions{UID: &dep.obj.UID}})
	if err != nil && !client.IsNotFound(err) && !client.IsConflict(err) {
		log.Printf("gc: delete orphaned %s/%s: %v", dep.res.Resource, dep.obj.Name, err)
	}
}
