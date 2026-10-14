package informer_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"mini-k8s/internal/api"
	"mini-k8s/internal/client"
	"mini-k8s/internal/informer"
	"mini-k8s/internal/testenv"
)

type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, s)
}

func (r *recorder) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, e := range r.events {
			if e == want {
				r.mu.Unlock()
				return
			}
		}
		r.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("event %q not observed; got %v", want, r.events)
}

func TestInformerCachesAndNotifies(t *testing.T) {
	c := testenv.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pods := client.Pods(c, "default")
	newPod := func(name string) *api.Pod {
		return &api.Pod{ObjectMeta: api.ObjectMeta{Name: name, Labels: map[string]string{"app": name}},
			Spec: api.PodSpec{Containers: []api.Container{{Name: "c", Image: "i"}}}}
	}
	if _, err := pods.Create(ctx, newPod("before")); err != nil {
		t.Fatal(err)
	}
	inf := informer.New(client.Pods(c, ""), client.ListOptions{}, 0)
	rec := &recorder{}
	inf.AddHandler(informer.Handler[*api.Pod]{
		OnAdd:    func(p *api.Pod) { rec.add("add " + p.Name) },
		OnUpdate: func(old, cur *api.Pod) { rec.add("update " + cur.Name + " " + cur.Labels["app"]) },
		OnDelete: func(p *api.Pod) { rec.add("delete " + p.Name) },
	})
	go inf.Run(ctx)
	if !informer.WaitForSync(ctx, inf.HasSynced) {
		t.Fatal("not synced")
	}
	rec.waitFor(t, "add before")

	p, err := pods.Create(ctx, newPod("after"))
	if err != nil {
		t.Fatal(err)
	}
	rec.waitFor(t, "add after")
	p.Labels["app"] = "changed"
	if _, err := pods.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	rec.waitFor(t, "update after changed")
	if got, ok := inf.Get("default", "after"); !ok || got.Labels["app"] != "changed" {
		t.Fatalf("cache not updated: %+v", got)
	}
	sel, _ := api.ParseSelector("app=before")
	if items := inf.List("default", sel); len(items) != 1 || items[0].Name != "before" {
		t.Fatalf("List with selector: %v", items)
	}
	if err := pods.Delete(ctx, "after", nil); err != nil {
		t.Fatal(err)
	}
	rec.waitFor(t, "delete after")
	if _, ok := inf.Get("default", "after"); ok {
		t.Fatal("deleted object still cached")
	}
}

func TestKeyRoundTrip(t *testing.T) {
	p := &api.Pod{ObjectMeta: api.ObjectMeta{Name: "a", Namespace: "ns"}}
	if informer.Key(p) != "ns/a" {
		t.Fatal(informer.Key(p))
	}
	if ns, name := informer.SplitKey("ns/a"); ns != "ns" || name != "a" {
		t.Fatal(ns, name)
	}
	if ns, name := informer.SplitKey("node-1"); ns != "" || name != "node-1" {
		t.Fatal(ns, name)
	}
}
