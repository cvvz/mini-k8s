// Package record emits Events about objects. Repeated identical events are
// aggregated into one Event whose count grows, like client-go's recorder.
package record

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
)

// Recorder sends Events asynchronously; it never blocks the caller.
type Recorder struct {
	c      *client.Client
	source corev1.EventSource
	queue  chan *corev1.Event

	mu   sync.Mutex
	seen map[string]seenEvent
}

type seenEvent struct {
	namespace, name string
	at              time.Time
}

// aggregationWindow is how long identical events are merged.
const aggregationWindow = 10 * time.Minute

// New returns a Recorder reporting as component on host. Call Run.
func New(c *client.Client, component, host string) *Recorder {
	return &Recorder{c: c, source: corev1.EventSource{Component: component, Host: host},
		queue: make(chan *corev1.Event, 256), seen: map[string]seenEvent{}}
}

// Eventf records an event about obj. eventType is Normal or Warning.
func (r *Recorder) Eventf(obj metav1.Object, eventType, reason, format string, args ...any) {
	t, m := obj.GetTypeMeta(), obj.GetObjectMeta()
	ns := m.Namespace
	if ns == "" {
		ns = corev1.NamespaceDefault
	}
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s.%x", m.Name, time.Now().UnixNano()), Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: t.Kind, APIVersion: t.APIVersion, Namespace: m.Namespace,
			Name: m.Name, UID: m.UID},
		Reason: reason, Message: fmt.Sprintf(format, args...), Type: eventType,
		Source: r.source, FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}
	select {
	case r.queue <- ev:
	default:
		log.Printf("event queue full; dropping %s %s: %s", reason, m.Name, ev.Message)
	}
}

// Run writes queued events until ctx ends.
func (r *Recorder) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-r.queue:
			if err := r.write(ctx, ev); err != nil && ctx.Err() == nil {
				log.Printf("record event %s/%s: %v", ev.InvolvedObject.Name, ev.Reason, err)
			}
		}
	}
}

func (r *Recorder) write(ctx context.Context, ev *corev1.Event) error {
	key := ev.InvolvedObject.UID + "|" + ev.Type + "|" + ev.Reason + "|" + ev.Message
	events := client.Events(r.c, ev.Namespace)
	r.mu.Lock()
	prev, ok := r.seen[key]
	r.mu.Unlock()
	if ok && time.Since(prev.at) < aggregationWindow {
		existing, err := events.Get(ctx, prev.name)
		if err == nil {
			existing.Count++
			existing.LastTimestamp = ev.LastTimestamp
			if _, err = events.Update(ctx, existing); err == nil {
				r.remember(key, prev.namespace, prev.name)
				return nil
			}
		}
		if !client.IsNotFound(err) && !client.IsConflict(err) {
			return err
		}
	}
	created, err := events.Create(ctx, ev)
	if err != nil {
		return err
	}
	r.remember(key, created.Namespace, created.Name)
	return nil
}

func (r *Recorder) remember(key, namespace, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) > 4096 {
		r.seen = map[string]seenEvent{}
	}
	r.seen[key] = seenEvent{namespace: namespace, name: name, at: time.Now()}
}
