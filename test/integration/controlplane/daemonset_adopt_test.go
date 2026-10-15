package controlplane

import (
	"context"
	"testing"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/controller"
	"mini-k8s/pkg/controller/daemon"
	"mini-k8s/pkg/controller/history"
	"mini-k8s/test/integration/framework"
)

// A DaemonSet created again after an orphaning delete adopts its old
// revision and Pods instead of running a second Pod per node; a revision
// name taken by a foreign template is a collision, resolved by hashing
// with the collision count.
func TestDaemonSetAdoptsOrphansAndResolvesCollisions(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, n := range []string{"w1", "w2"} {
		if _, err := client.Nodes(c).Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n}}); err != nil {
			t.Fatal(err)
		}
		go heartbeat(ctx, c, n)
	}
	tpl := func(app string) corev1.PodTemplateSpec {
		t := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": app}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}
		corev1.SetDefaultsPodSpec(&t.Spec)
		return t
	}
	selector := func(app string) *metav1.LabelSelector {
		return &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}}
	}

	// Orphans of an "agent" DaemonSet: its revision and its Pod on w1.
	agent := tpl("agent")
	hash := controller.ComputeHash(&agent)
	orphanRev := &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: "agent-" + hash,
		Labels: map[string]string{"app": "agent", appsv1.ControllerRevisionHashLabelKey: hash}}, Data: history.RevisionData(&agent), Revision: 1}
	if _, err := client.ControllerRevisions(c, "default").Create(ctx, orphanRev); err != nil {
		t.Fatal(err)
	}
	orphanPod := daemon.NewPod(&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"},
		Spec: appsv1.DaemonSetSpec{Template: agent}}, "w1", hash)
	orphanPod.OwnerReferences = nil
	orphanPod.Spec.NodeName = "w1"
	if _, err := client.Pods(c, "default").Create(ctx, orphanPod); err != nil {
		t.Fatal(err)
	}
	// A revision name that a "victim" DaemonSet will want, taken by an
	// unrelated template its selector does not match.
	victim := tpl("victim")
	victimHash := controller.ComputeHash(&victim)
	squatter := tpl("squatter")
	if _, err := client.ControllerRevisions(c, "default").Create(ctx, &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "victim-" + victimHash, Labels: map[string]string{"app": "squatter"}},
		Data:       history.RevisionData(&squatter), Revision: 1}); err != nil {
		t.Fatal(err)
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

	for _, app := range []string{"agent", "victim"} {
		if _, err := client.DaemonSets(c, "default").Create(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: app},
			Spec: appsv1.DaemonSetSpec{Selector: selector(app), Template: tpl(app)}}); err != nil {
			t.Fatal(err)
		}
	}
	podsOf := func(app string) map[string]int {
		list, _ := client.Pods(c, "default").List(ctx, client.ListOptions{LabelSelector: "app=" + app})
		out := map[string]int{}
		for _, p := range list.Items {
			if metav1.ControllerOf(&p.ObjectMeta) != nil {
				out[daemonPodNode(&p)]++
			}
		}
		return out
	}
	waitFor(t, "agent to adopt its Pod and add one on w2", func() bool {
		got := podsOf("agent")
		return got["w1"] == 1 && got["w2"] == 1
	})
	rev, err := client.ControllerRevisions(c, "default").Get(ctx, "agent-"+hash)
	if err != nil || metav1.ControllerOf(&rev.ObjectMeta) == nil || rev.Revision != 1 {
		t.Fatalf("the orphaned revision is adopted: %+v %v", rev, err)
	}
	list, _ := client.Pods(c, "default").List(ctx, client.ListOptions{LabelSelector: "app=agent"})
	if len(list.Items) != 2 {
		t.Fatalf("no second Pod on w1: %d Pods", len(list.Items))
	}

	waitFor(t, "victim to resolve the collision", func() bool {
		ds, err := client.DaemonSets(c, "default").Get(ctx, "victim")
		got := podsOf("victim")
		return err == nil && ds.Status.CollisionCount != nil && *ds.Status.CollisionCount == 1 && got["w1"] == 1 && got["w2"] == 1
	})
	squat, err := client.ControllerRevisions(c, "default").Get(ctx, "victim-"+victimHash)
	if err != nil || metav1.ControllerOf(&squat.ObjectMeta) != nil {
		t.Fatalf("the foreign revision is left alone: %+v %v", squat, err)
	}
}

// daemonPodNode is the node a daemon Pod is bound or pinned to.
func daemonPodNode(p *corev1.Pod) string {
	if p.Spec.NodeName != "" {
		return p.Spec.NodeName
	}
	return p.Spec.Affinity.NodeAffinity.Required.NodeSelectorTerms[0].MatchFields[0].Values[0]
}
