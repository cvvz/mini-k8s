package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/controller/nodelifecycle"
	"mini-k8s/pkg/controller/tainteviction"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/test/integration/framework"
)

func TestLostNodeIsTaintedAndItsPodsEvicted(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := client.Nodes(c).Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "lost"}}); err != nil {
		t.Fatal(err)
	}
	beat, stopBeat := context.WithCancel(ctx)
	go heartbeat(beat, c, "lost")

	rec := record.New(c, "test", "")
	go rec.Run(ctx)
	nodes := informers.New(client.Nodes(c), client.ListOptions{}, 0)
	pods := informers.New(client.Pods(c, ""), client.ListOptions{}, 0)
	leases := informers.New(client.Leases(c, corev1.NamespaceNodeLease), client.ListOptions{}, 0)
	lifecycle := nodelifecycle.NewController(c, nodes, leases, pods, rec)
	lifecycle.Grace, lifecycle.Period = 3*time.Second, 200*time.Millisecond
	eviction := tainteviction.NewController(c, nodes, pods, rec)
	for _, run := range []func(context.Context){nodes.Run, pods.Run, leases.Run} {
		go run(ctx)
	}
	informers.WaitForSync(ctx, nodes.HasSynced, pods.HasSynced, leases.HasSynced)
	go lifecycle.Run(ctx)
	go eviction.Run(ctx, 1)

	// Two running, ready Pods on the node: one tolerates its loss for 2s,
	// the other gets the default 300s.
	mk := func(name string, tolerations []corev1.Toleration) {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PodSpec{NodeName: "lost", Tolerations: tolerations, Containers: []corev1.Container{{Name: "c", Image: "img"}}}}
		p, err := client.Pods(c, "default").Create(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		p.Status.Phase = corev1.PodRunning
		p.Status.SetCondition(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue})
		if _, err := client.Pods(c, "default").UpdateStatus(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	mk("short", []corev1.Toleration{{Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists,
		Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To[int64](2)}})
	mk("default", nil)
	get := func(name string) *corev1.Pod {
		p, err := client.Pods(c, "default").Get(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if tol := get("default").Spec.Tolerations; len(tol) != 2 || *tol[1].TolerationSeconds != 300 {
		t.Fatalf("default tolerations: %+v", tol)
	}

	stopBeat()
	eventually(t, "the node is Unknown and tainted unreachable", func() (bool, string) {
		n, err := client.Nodes(c).Get(ctx, "lost")
		if err != nil {
			return false, err.Error()
		}
		ready := n.Status.Condition(corev1.NodeReady)
		keys := map[string]bool{}
		for _, taint := range n.Spec.Taints {
			keys[taint.Key+":"+taint.Effect] = true
		}
		return ready.Status == corev1.ConditionUnknown && ready.Reason == "NodeStatusUnknown" &&
			keys[corev1.TaintNodeUnreachable+":NoSchedule"] && keys[corev1.TaintNodeUnreachable+":NoExecute"], fmt.Sprintf("%+v %v", ready, keys)
	})
	eventually(t, "the Pods are not ready and the short-tolerating one is evicted", func() (bool, string) {
		short, def := get("short"), get("default")
		return !corev1.IsPodReady(def) && short.DeletionTimestamp != nil && def.DeletionTimestamp == nil,
			fmt.Sprintf("short deleting=%v default ready=%v", short.DeletionTimestamp != nil, corev1.IsPodReady(def))
	})

	beat, stopBeat = context.WithCancel(ctx)
	defer stopBeat()
	go heartbeat(beat, c, "lost")
	eventually(t, "the node is Ready again and untainted", func() (bool, string) {
		n, err := client.Nodes(c).Get(ctx, "lost")
		if err != nil {
			return false, err.Error()
		}
		return corev1.IsNodeReady(n) && len(n.Spec.Taints) == 0, fmt.Sprintf("%+v", n.Spec.Taints)
	})
	if get("default").DeletionTimestamp != nil {
		t.Fatal("the Pod with the default toleration must survive a short outage")
	}
}
