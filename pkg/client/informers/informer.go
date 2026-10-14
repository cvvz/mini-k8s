// Package informer keeps a local, watch-driven cache of one resource and
// dispatches add/update/delete notifications, like client-go's
// SharedIndexInformer. Cached objects are shared: callers must DeepCopy
// before modifying them.
package informers

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"mini-k8s/pkg/client"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/labels"
	"mini-k8s/pkg/watch"
)

// Handler receives notifications. Nil callbacks are skipped.
type Handler[PT any] struct {
	OnAdd    func(obj PT)
	OnUpdate func(old, cur PT)
	OnDelete func(obj PT)
}

// Informer caches objects of one resource.
type Informer[T any, PT client.ObjectPtr[T]] struct {
	res    client.Resource[T, PT]
	opts   client.ListOptions
	resync time.Duration

	mu       sync.RWMutex
	items    map[string]PT
	handlers []Handler[PT]
	synced   atomic.Bool
}

// New creates an informer. resync > 0 periodically replays every cached
// object as an update so controllers re-check their state.
func New[T any, PT client.ObjectPtr[T]](res client.Resource[T, PT], opts client.ListOptions, resync time.Duration) *Informer[T, PT] {
	return &Informer[T, PT]{res: res, opts: opts, resync: resync, items: map[string]PT{}}
}

// Key returns "namespace/name", or "name" for cluster-scoped objects.
func Key(obj metav1.Object) string {
	m := obj.GetObjectMeta()
	if m.Namespace == "" {
		return m.Name
	}
	return m.Namespace + "/" + m.Name
}

// SplitKey reverses Key.
func SplitKey(key string) (namespace, name string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return "", key
}

// AddHandler registers callbacks; call it before Run.
func (i *Informer[T, PT]) AddHandler(h Handler[PT]) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.handlers = append(i.handlers, h)
}

// HasSynced reports whether the initial list has been loaded.
func (i *Informer[T, PT]) HasSynced() bool { return i.synced.Load() }

// Get returns the cached object.
func (i *Informer[T, PT]) Get(namespace, name string) (PT, bool) {
	key := name
	if namespace != "" {
		key = namespace + "/" + name
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	obj, ok := i.items[key]
	return obj, ok
}

// List returns cached objects in namespace ("" = all) matching sel (nil = all).
func (i *Informer[T, PT]) List(namespace string, sel labels.Selector) []PT {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]PT, 0, len(i.items))
	for _, obj := range i.items {
		m := obj.GetObjectMeta()
		if (namespace == "" || m.Namespace == namespace) && sel.Matches(m.Labels) {
			out = append(out, obj)
		}
	}
	return out
}

// Run lists, then watches, relisting when the watch falls too far behind.
func (i *Informer[T, PT]) Run(ctx context.Context) {
	if i.resync > 0 {
		go i.resyncLoop(ctx)
	}
	for ctx.Err() == nil {
		rv, err := i.relist(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("informer %s: list failed: %v", i.res.Info.Resource, err)
				sleep(ctx, time.Second)
			}
			continue
		}
		i.synced.Store(true)
		i.watch(ctx, rv)
	}
}

func (i *Informer[T, PT]) relist(ctx context.Context) (string, error) {
	list, err := i.res.List(ctx, i.opts)
	if err != nil {
		return "", err
	}
	fresh := make(map[string]PT, len(list.Items))
	for idx := range list.Items {
		obj := PT(&list.Items[idx])
		fresh[Key(obj)] = obj
	}
	i.mu.Lock()
	old := i.items
	i.items = fresh
	handlers := i.handlers
	i.mu.Unlock()
	for key, obj := range fresh {
		if prev, ok := old[key]; ok {
			if prev.GetObjectMeta().ResourceVersion != obj.GetObjectMeta().ResourceVersion {
				notifyUpdate(handlers, prev, obj)
			}
		} else {
			notifyAdd(handlers, obj)
		}
	}
	for key, obj := range old {
		if _, ok := fresh[key]; !ok {
			notifyDelete(handlers, obj)
		}
	}
	return list.ListMeta.ResourceVersion, nil
}

// watch consumes watch streams from rv until a relist is required.
func (i *Informer[T, PT]) watch(ctx context.Context, rv string) {
	for ctx.Err() == nil {
		opts := i.opts
		opts.ResourceVersion = rv
		w, err := i.res.Watch(ctx, opts)
		if client.IsExpired(err) {
			return
		}
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("informer %s: watch failed: %v", i.res.Info.Resource, err)
				sleep(ctx, time.Second)
			}
			continue
		}
		expired := false
		for ev := range w.Events() {
			if ev.Type == watch.Error {
				expired = true
				break
			}
			rv = ev.Object.GetObjectMeta().ResourceVersion
			i.apply(ev.Type, ev.Object)
		}
		w.Stop() // releases the stream's context on every exit path
		if expired || client.IsExpired(w.Err()) {
			return
		}
	}
}

func (i *Informer[T, PT]) apply(typ string, obj PT) {
	key := Key(obj)
	i.mu.Lock()
	prev, existed := i.items[key]
	if typ == watch.Deleted {
		delete(i.items, key)
	} else {
		i.items[key] = obj
	}
	handlers := i.handlers
	i.mu.Unlock()
	switch {
	case typ == watch.Deleted:
		notifyDelete(handlers, obj)
	case existed:
		notifyUpdate(handlers, prev, obj)
	default:
		notifyAdd(handlers, obj)
	}
}

func (i *Informer[T, PT]) resyncLoop(ctx context.Context) {
	t := time.NewTicker(i.resync)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		i.mu.RLock()
		objs := make([]PT, 0, len(i.items))
		for _, obj := range i.items {
			objs = append(objs, obj)
		}
		handlers := i.handlers
		i.mu.RUnlock()
		for _, obj := range objs {
			notifyUpdate(handlers, obj, obj)
		}
	}
}

func notifyAdd[PT any](hs []Handler[PT], obj PT) {
	for _, h := range hs {
		if h.OnAdd != nil {
			h.OnAdd(obj)
		}
	}
}

func notifyUpdate[PT any](hs []Handler[PT], old, cur PT) {
	for _, h := range hs {
		if h.OnUpdate != nil {
			h.OnUpdate(old, cur)
		}
	}
}

func notifyDelete[PT any](hs []Handler[PT], obj PT) {
	for _, h := range hs {
		if h.OnDelete != nil {
			h.OnDelete(obj)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// WaitForSync blocks until every informer has synced or ctx ends.
func WaitForSync(ctx context.Context, synced ...func() bool) bool {
	for {
		all := true
		for _, s := range synced {
			if !s() {
				all = false
			}
		}
		if all {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}
