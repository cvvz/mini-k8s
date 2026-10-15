package controlplane

import (
	"context"
	"sort"
	"testing"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/controller/daemon"
	"mini-k8s/pkg/scheduler"
	"mini-k8s/test/integration/framework"
)

func TestDaemonSetRolloutAndRollback(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, n := range []string{"w1", "w2", "w3"} {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n, Labels: map[string]string{corev1.LabelHostname: n}}}
		if n == "w3" {
			node.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "db", Effect: corev1.TaintEffectNoSchedule}}
		}
		if _, err := client.Nodes(c).Create(ctx, node); err != nil {
			t.Fatal(err)
		}
		go heartbeat(ctx, c, n)
	}
	rec := record.New(c, "test", "")
	go rec.Run(ctx)
	pods := informers.New(client.Pods(c, ""), client.ListOptions{}, 0)
	nodes := informers.New(client.Nodes(c), client.ListOptions{}, 0)
	dsets := informers.New(client.DaemonSets(c, ""), client.ListOptions{}, 0)
	revisions := informers.New(client.ControllerRevisions(c, ""), client.ListOptions{}, 0)
	for _, run := range []func(context.Context){pods.Run, nodes.Run, dsets.Run, revisions.Run} {
		go run(ctx)
	}
	informers.WaitForSync(ctx, pods.HasSynced, nodes.HasSynced, dsets.HasSynced, revisions.HasSynced)
	go daemon.NewController(c, dsets, pods, nodes, revisions, rec).Run(ctx, 1)
	go scheduler.New(c, pods, nodes, rec).Run(ctx)
	go fakeKubelet(ctx, c)

	tpl := func(value string) corev1.PodTemplateSpec {
		return corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "agent"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img", Env: []corev1.EnvVar{{Name: "V", Value: value}}}}}}
	}
	dss := client.DaemonSets(c, "default")
	ds, err := dss.Create(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent"}, Spec: appsv1.DaemonSetSpec{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "agent"}}, Template: tpl("1")}})
	if err != nil {
		t.Fatal(err)
	}
	if ds.Annotations[appsv1.DeprecatedTemplateGeneration] != "1" || ds.Spec.UpdateStrategy.Type != "RollingUpdate" {
		t.Fatalf("defaults: %+v", ds)
	}
	// running returns the node of every live Pod and the revision it runs.
	running := func() map[string]string {
		list, err := client.Pods(c, "default").List(ctx, client.ListOptions{})
		if err != nil {
			return nil
		}
		out := map[string]string{}
		for _, p := range list.Items {
			if p.DeletionTimestamp == nil && p.Spec.NodeName != "" && corev1.IsPodReady(&p) {
				out[p.Spec.NodeName] = p.Labels[appsv1.ControllerRevisionHashLabelKey]
			}
		}
		return out
	}
	settled := func(what string, hash *string) {
		t.Helper()
		waitFor(t, what, func() bool {
			got, err := dss.Get(ctx, "agent")
			if err != nil || got.Status.ObservedGeneration != got.Generation || got.Status.DesiredNumberScheduled != 2 ||
				got.Status.UpdatedNumberScheduled != 2 || got.Status.NumberAvailable != 2 {
				return false
			}
			r := running()
			if len(r) != 2 || r["w1"] == "" || r["w1"] != r["w2"] {
				return false
			}
			*hash = r["w1"]
			return true
		})
	}
	var first, second, third string
	settled("a Pod on each untainted node", &first)

	revisionsOf := func() map[string]int64 {
		list, _ := client.ControllerRevisions(c, "default").List(ctx, client.ListOptions{})
		out := map[string]int64{}
		for _, cr := range list.Items {
			out[cr.Labels[appsv1.ControllerRevisionHashLabelKey]] = cr.Revision
		}
		return out
	}
	if r := revisionsOf(); len(r) != 1 || r[first] != 1 {
		t.Fatalf("one revision: %v", r)
	}

	ds, _ = dss.Get(ctx, "agent")
	ds.Spec.Template = tpl("2")
	if ds, err = dss.Update(ctx, ds); err != nil {
		t.Fatal(err)
	}
	if ds.Annotations[appsv1.DeprecatedTemplateGeneration] != "2" {
		t.Fatalf("template generation: %v", ds.Annotations)
	}
	settled("the rollout", &second)
	if second == first {
		t.Fatal("the Pods must be replaced")
	}
	list, _ := client.Pods(c, "default").List(ctx, client.ListOptions{})
	for _, p := range list.Items {
		if p.DeletionTimestamp == nil && p.Labels[appsv1.DaemonSetTemplateGenerationKey] != "2" {
			t.Errorf("Pod %s has generation %q", p.Name, p.Labels[appsv1.DaemonSetTemplateGenerationKey])
		}
	}

	// Rolling back reuses the first revision, now numbered 3.
	ds, _ = dss.Get(ctx, "agent")
	ds.Spec.Template = tpl("1")
	if _, err = dss.Update(ctx, ds); err != nil {
		t.Fatal(err)
	}
	settled("the rollback", &third)
	if r := revisionsOf(); third != first || len(r) != 2 || r[first] != 3 || r[second] != 2 {
		t.Fatalf("revisions after rollback: %v (hash %s, first %s)", r, third, first)
	}

	// A deleted Pod comes back; the tainted node never gets one.
	before := running()
	for _, p := range list.Items {
		if p.Spec.NodeName == "w1" && p.DeletionTimestamp == nil {
			_ = client.Pods(c, "default").Delete(ctx, p.Name, nil)
		}
	}
	waitFor(t, "the Pod on w1 to come back", func() bool {
		r := running()
		return len(r) == 2 && r["w1"] == before["w1"]
	})
	got, _ := dss.Get(ctx, "agent")
	if got.Status.NumberMisscheduled != 0 || got.Status.CurrentNumberScheduled != 2 {
		t.Fatalf("status: %+v", got.Status)
	}
	var nodesWithPods []string
	for n := range running() {
		nodesWithPods = append(nodesWithPods, n)
	}
	sort.Strings(nodesWithPods)
	if len(nodesWithPods) != 2 || nodesWithPods[0] != "w1" || nodesWithPods[1] != "w2" {
		t.Fatalf("nodes with Pods: %v", nodesWithPods)
	}
}
