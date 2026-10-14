package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"mini-k8s/internal/api"
)

// ObjectPtr constrains PT to be a pointer to an API object type T.
type ObjectPtr[T any] interface {
	*T
	api.Object
}

// Resource is a typed client for one resource in one namespace (or all
// namespaces when the namespace is empty).
type Resource[T any, PT ObjectPtr[T]] struct {
	c    *Client
	Info api.ResourceInfo
	ns   string
}

// For returns a typed client; the type parameter PT is inferred.
func For[T any, PT ObjectPtr[T]](c *Client, info api.ResourceInfo, namespace string) Resource[T, PT] {
	return Resource[T, PT]{c: c, Info: info, ns: namespace}
}

// Typed clients for registered resources.
func Namespaces(c *Client) Resource[api.Namespace, *api.Namespace] {
	return For[api.Namespace](c, api.Namespaces, "")
}

// Nodes returns a Node client.
func Nodes(c *Client) Resource[api.Node, *api.Node] { return For[api.Node](c, api.Nodes, "") }

// Pods returns a Pod client for namespace ("" = all namespaces, read only).
func Pods(c *Client, ns string) Resource[api.Pod, *api.Pod] { return For[api.Pod](c, api.Pods, ns) }

// Services returns a Service client.
func Services(c *Client, ns string) Resource[api.Service, *api.Service] {
	return For[api.Service](c, api.Services, ns)
}

// Events returns an Event client.
func Events(c *Client, ns string) Resource[api.Event, *api.Event] {
	return For[api.Event](c, api.Events, ns)
}

// Deployments returns a Deployment client.
func Deployments(c *Client, ns string) Resource[api.Deployment, *api.Deployment] {
	return For[api.Deployment](c, api.Deployments, ns)
}

// InNamespace returns a copy of the client bound to ns.
func (r Resource[T, PT]) InNamespace(ns string) Resource[T, PT] {
	r.ns = ns
	return r
}

func (r Resource[T, PT]) namespaceOf(obj PT) string {
	if ns := obj.GetObjectMeta().Namespace; ns != "" {
		return ns
	}
	return r.ns
}

// Get fetches one object.
func (r Resource[T, PT]) Get(ctx context.Context, name string) (PT, error) {
	out := PT(new(T))
	err := r.c.Do(ctx, http.MethodGet, r.Info.Path(r.ns, name), nil, "", nil, out)
	return out, err
}

// List fetches matching objects and the list resourceVersion.
func (r Resource[T, PT]) List(ctx context.Context, opts ListOptions) (*api.List[T], error) {
	out := &api.List[T]{}
	err := r.c.Do(ctx, http.MethodGet, r.Info.Path(r.ns, ""), opts.Query(), "", nil, out)
	return out, err
}

// Create stores a new object.
func (r Resource[T, PT]) Create(ctx context.Context, obj PT) (PT, error) {
	r.Info.SetTypeMeta(obj)
	out := PT(new(T))
	err := r.c.Do(ctx, http.MethodPost, r.Info.Path(r.namespaceOf(obj), ""), nil, "", obj, out)
	return out, err
}

// Update replaces an object; it fails with a conflict when obj is stale.
func (r Resource[T, PT]) Update(ctx context.Context, obj PT) (PT, error) {
	return r.put(ctx, obj, "")
}

// UpdateStatus replaces only the status of an object.
func (r Resource[T, PT]) UpdateStatus(ctx context.Context, obj PT) (PT, error) {
	return r.put(ctx, obj, "status")
}

func (r Resource[T, PT]) put(ctx context.Context, obj PT, sub string) (PT, error) {
	r.Info.SetTypeMeta(obj)
	out := PT(new(T))
	var subs []string
	if sub != "" {
		subs = append(subs, sub)
	}
	err := r.c.Do(ctx, http.MethodPut, r.Info.Path(r.namespaceOf(obj), obj.GetObjectMeta().Name, subs...), nil, "", obj, out)
	return out, err
}

// Patch applies a JSON merge patch (a value marshalled to JSON, or []byte).
// Including metadata.resourceVersion in the patch makes it conditional.
func (r Resource[T, PT]) Patch(ctx context.Context, name string, patch any, subresource ...string) (PT, error) {
	out := PT(new(T))
	err := r.c.Do(ctx, http.MethodPatch, r.Info.Path(r.ns, name, subresource...), nil, "application/merge-patch+json", patch, out)
	return out, err
}

// Delete deletes an object; opts may be nil.
func (r Resource[T, PT]) Delete(ctx context.Context, name string, opts *api.DeleteOptions) error {
	var body any
	if opts != nil {
		body = opts
	}
	return r.c.Do(ctx, http.MethodDelete, r.Info.Path(r.ns, name), nil, "", body, nil)
}

// Event is one decoded watch event. Status is set for ERROR events.
type Event[PT any] struct {
	Type   string
	Object PT
	Status *api.Status
}

// Watcher delivers events until the stream ends; Err reports why.
type Watcher[PT any] struct {
	events chan Event[PT]
	cancel context.CancelFunc
	err    error
}

// Events returns the event channel; it is closed when the watch ends.
func (w *Watcher[PT]) Events() <-chan Event[PT] { return w.events }

// Stop ends the watch.
func (w *Watcher[PT]) Stop() { w.cancel() }

// Err is valid after Events is closed.
func (w *Watcher[PT]) Err() error { return w.err }

// Watch opens a watch stream. Without opts.ResourceVersion the stream
// starts with ADDED events for existing objects.
func (r Resource[T, PT]) Watch(ctx context.Context, opts ListOptions) (*Watcher[PT], error) {
	ctx, cancel := context.WithCancel(ctx)
	q := opts.Query()
	q.Set("watch", "true")
	resp, err := r.c.Request(ctx, http.MethodGet, r.Info.Path(r.ns, ""), q, "", nil, true)
	if err != nil {
		cancel()
		return nil, err
	}
	w := &Watcher[PT]{events: make(chan Event[PT]), cancel: cancel}
	go func() {
		defer close(w.events)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64<<10), 16<<20)
		for scanner.Scan() {
			var raw api.WatchEvent
			if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
				w.err = fmt.Errorf("decode watch event: %w", err)
				return
			}
			ev := Event[PT]{Type: raw.Type}
			if raw.Type == api.EventError {
				st := &api.Status{}
				_ = json.Unmarshal(raw.Object, st)
				ev.Status = st
				w.err = &APIError{Status: *st}
			} else {
				ev.Object = PT(new(T))
				if err := json.Unmarshal(raw.Object, ev.Object); err != nil {
					w.err = fmt.Errorf("decode watch object: %w", err)
					return
				}
			}
			select {
			case w.events <- ev:
			case <-ctx.Done():
				return
			}
			if ev.Status != nil {
				return
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			w.err = err
		}
	}()
	return w, nil
}
