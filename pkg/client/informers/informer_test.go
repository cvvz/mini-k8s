package informers_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/test/integration/framework"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/labels"
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
	c := framework.StartTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pods := client.Pods(c, "default")
	newPod := func(name string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app": name}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}}}
	}
	if _, err := pods.Create(ctx, newPod("before")); err != nil {
		t.Fatal(err)
	}
	inf := informers.New(client.Pods(c, ""), client.ListOptions{}, 0)
	rec := &recorder{}
	inf.AddHandler(informers.Handler[*corev1.Pod]{
		OnAdd:    func(p *corev1.Pod) { rec.add("add " + p.Name) },
		OnUpdate: func(old, cur *corev1.Pod) { rec.add("update " + cur.Name + " " + cur.Labels["app"]) },
		OnDelete: func(p *corev1.Pod) { rec.add("delete " + p.Name) },
	})
	go inf.Run(ctx)
	if !informers.WaitForSync(ctx, inf.HasSynced) {
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
	sel, _ := labels.Parse("app=before")
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
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"}}
	if informers.Key(p) != "ns/a" {
		t.Fatal(informers.Key(p))
	}
	if ns, name := informers.SplitKey("ns/a"); ns != "ns" || name != "a" {
		t.Fatal(ns, name)
	}
	if ns, name := informers.SplitKey("node-1"); ns != "" || name != "node-1" {
		t.Fatal(ns, name)
	}
}
