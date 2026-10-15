package podgc

import (
	"context"
	"testing"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/test/integration/framework"
)

func TestOrphanedPodsAreFailedAndDeleted(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	createNode := func(name string) {
		t.Helper()
		if _, err := client.Nodes(c).Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	createPod := func(name, node, phase string, finalizers ...string) {
		t.Helper()
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: finalizers},
			Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Image: "img"}}}}
		p, err := client.Pods(c, "default").Create(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		p.Status.Phase = phase
		if _, err := client.Pods(c, "default").UpdateStatus(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	createNode("kept")
	createNode("gone")
	createPod("on-kept", "kept", corev1.PodRunning)
	createPod("unscheduled", "", corev1.PodPending)
	createPod("on-gone", "gone", corev1.PodRunning)
	createPod("done-on-gone", "gone", corev1.PodSucceeded)
	createPod("held", "gone", corev1.PodRunning, "e2e.mini-k8s.io/hold")
	createPod("on-flaky", "flaky", corev1.PodRunning)

	nodes := informers.New(client.Nodes(c), client.ListOptions{}, 0)
	pods := informers.New(client.Pods(c, ""), client.ListOptions{}, 0)
	go nodes.Run(ctx)
	go pods.Run(ctx)
	informers.WaitForSync(ctx, nodes.HasSynced, pods.HasSynced)
	gc := NewController(c, pods, nodes)
	now := time.Unix(1000, 0)
	gc.now = func() time.Time { return now }
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	exists := func(name string) bool {
		_, err := client.Pods(c, "default").Get(ctx, name)
		return err == nil
	}

	if err := client.Nodes(c).Delete(ctx, "gone", nil); err != nil {
		t.Fatal(err)
	}
	waitFor("the informer to drop the node", func() bool { _, ok := nodes.Get("", "gone"); return !ok })
	gc.gcOrphaned(ctx)
	now = now.Add(39 * time.Second)
	gc.gcOrphaned(ctx)
	for _, name := range []string{"on-gone", "done-on-gone", "held"} {
		if !exists(name) {
			t.Fatalf("%s deleted within the quarantine", name)
		}
	}
	// flaky registers within the quarantine: its Pod stays.
	createNode("flaky")
	waitFor("the informer to see flaky", func() bool { _, ok := nodes.Get("", "flaky"); return ok })

	now = now.Add(2 * time.Second)
	gc.gcOrphaned(ctx)
	for _, name := range []string{"on-gone", "done-on-gone"} {
		if exists(name) {
			t.Errorf("%s survived its node", name)
		}
	}
	for _, name := range []string{"on-kept", "unscheduled", "on-flaky"} {
		if !exists(name) {
			t.Errorf("%s was deleted", name)
		}
	}
	held, err := client.Pods(c, "default").Get(ctx, "held")
	if err != nil {
		t.Fatal(err)
	}
	cond := held.Status.Condition(corev1.DisruptionTarget)
	if held.DeletionTimestamp == nil || held.Status.Phase != corev1.PodFailed || cond == nil || cond.Status != corev1.ConditionTrue ||
		cond.Reason != "DeletionByPodGC" || cond.Message != "PodGC: node no longer exists" {
		t.Fatalf("held Pod: deleting=%v phase=%s condition=%+v", held.DeletionTimestamp != nil, held.Status.Phase, cond)
	}
	if len(gc.missing) != 1 {
		t.Fatalf("only gone is still tracked: %v", gc.missing)
	}
}

// A node the informer has not seen yet, but the API has, is not a vanished
// node.
func TestNodeTheAPIStillHasKeepsItsPods(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := client.Nodes(c).Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "new"}}); err != nil {
		t.Fatal(err)
	}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: corev1.PodSpec{NodeName: "new", Containers: []corev1.Container{{Name: "c", Image: "img"}}}}
	if _, err := client.Pods(c, "default").Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	pods := informers.New(client.Pods(c, ""), client.ListOptions{}, 0)
	go pods.Run(ctx)
	// The node informer never runs, so it never sees "new".
	nodes := informers.New(client.Nodes(c), client.ListOptions{}, 0)
	informers.WaitForSync(ctx, pods.HasSynced)
	gc := NewController(c, pods, nodes)
	now := time.Unix(1000, 0)
	gc.now = func() time.Time { return now }
	gc.gcOrphaned(ctx)
	now = now.Add(time.Minute)
	gc.gcOrphaned(ctx)
	if _, err := client.Pods(c, "default").Get(ctx, "p"); err != nil {
		t.Fatalf("the Pod of an existing node was deleted: %v", err)
	}
	if len(gc.missing) != 0 {
		t.Fatalf("a node the API has must be forgotten: %v", gc.missing)
	}
}
