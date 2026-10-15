package controlplane

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"mini-k8s/pkg/api/resource"
	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/controller/garbagecollector"
	"mini-k8s/pkg/controller/statefulset"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/test/integration/framework"
)

// startStatefulSetControllers runs the StatefulSet controller and the
// garbage collector. Nothing schedules the Pods; tests make them ready.
func startStatefulSetControllers(t *testing.T, c *client.Client) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rec := record.New(c, "test", "")
	go rec.Run(ctx)
	pods := informers.New(client.Pods(c, ""), client.ListOptions{}, 0)
	sets := informers.New(client.StatefulSets(c, ""), client.ListOptions{}, 0)
	claims := informers.New(client.PersistentVolumeClaims(c, ""), client.ListOptions{}, 0)
	revisions := informers.New(client.ControllerRevisions(c, ""), client.ListOptions{}, 0)
	for _, run := range []func(context.Context){pods.Run, sets.Run, claims.Run, revisions.Run} {
		go run(ctx)
	}
	informers.WaitForSync(ctx, pods.HasSynced, sets.HasSynced, claims.HasSynced, revisions.HasSynced)
	go statefulset.NewController(c, sets, pods, claims, revisions, rec).Run(ctx, 2)
	go garbagecollector.NewGarbageCollector(c, garbagecollector.TrackedResources()).Run(ctx)
	return ctx
}

// readyAll reports every live Pod Running and Ready, like a kubelet.
func readyAll(ctx context.Context, c *client.Client) {
	for ctx.Err() == nil {
		if list, err := client.Pods(c, "default").List(ctx, client.ListOptions{}); err == nil {
			for i := range list.Items {
				p := &list.Items[i]
				if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodPending {
					markReady(ctx, c, p)
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func markReady(ctx context.Context, c *client.Client, p *corev1.Pod) {
	p.Status.Phase = corev1.PodRunning
	p.Status.SetCondition(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue})
	_, _ = client.Pods(c, p.Namespace).UpdateStatus(ctx, p)
}

func newStatefulSet(name string, replicas int32, value string) *appsv1.StatefulSet {
	labels := map[string]string{"app": name}
	return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: appsv1.StatefulSetSpec{
		Replicas: ptr.To(replicas), ServiceName: name, Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "img", Env: []corev1.EnvVar{{Name: "V", Value: value}}}}}},
		VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"},
			Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []string{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResources{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}},
	}}
}

// setPods maps the names of a set's live Pods to their revision.
func setPods(ctx context.Context, c *client.Client, app string) map[string]string {
	list, err := client.Pods(c, "default").List(ctx, client.ListOptions{LabelSelector: "app=" + app})
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, p := range list.Items {
		if p.DeletionTimestamp == nil {
			out[p.Name] = p.Labels[appsv1.ControllerRevisionHashLabelKey]
		}
	}
	return out
}

func claimNames(ctx context.Context, c *client.Client) []string {
	list, err := client.PersistentVolumeClaims(c, "default").List(ctx, client.ListOptions{})
	if err != nil {
		return nil
	}
	var out []string
	for _, pvc := range list.Items {
		if pvc.DeletionTimestamp == nil {
			out = append(out, pvc.Name)
		}
	}
	sort.Strings(out)
	return out
}

func getSet(t *testing.T, ctx context.Context, c *client.Client, name string) *appsv1.StatefulSet {
	t.Helper()
	s, err := client.StatefulSets(c, "default").Get(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func updateSet(t *testing.T, ctx context.Context, c *client.Client, name string, change func(*appsv1.StatefulSet)) {
	t.Helper()
	s := getSet(t, ctx, c, name)
	change(s)
	if _, err := client.StatefulSets(c, "default").Update(ctx, s); err != nil {
		t.Fatal(err)
	}
}

// eventOrder lists the messages of the set's events with the given
// reason, oldest first.
func eventOrder(ctx context.Context, c *client.Client, name, reason string) []string {
	list, err := client.Events(c, "default").List(ctx, client.ListOptions{})
	if err != nil {
		return nil
	}
	sort.SliceStable(list.Items, func(i, j int) bool {
		return list.Items[i].FirstTimestamp.Before(list.Items[j].FirstTimestamp.Time)
	})
	var out []string
	for _, e := range list.Items {
		if e.InvolvedObject.Name == name && e.Reason == reason {
			out = append(out, e.Message)
		}
	}
	return out
}

func TestStatefulSetCreatesInOrderAndScalesDownFromTheTop(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startStatefulSetControllers(t, c)
	if _, err := client.StatefulSets(c, "default").Create(ctx, newStatefulSet("db", 3, "1")); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"db-0", "db-1", "db-2"} {
		waitFor(t, name, func() bool { return len(setPods(ctx, c, "db")) == i+1 })
		time.Sleep(300 * time.Millisecond)
		if n := len(setPods(ctx, c, "db")); n != i+1 {
			t.Fatalf("%d Pods before %s is ready", n, name)
		}
		p, err := client.Pods(c, "default").Get(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if p.Spec.Hostname != name || p.Spec.Subdomain != "db" || p.Labels[appsv1.StatefulSetPodNameLabel] != name ||
			p.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "data-"+name {
			t.Fatalf("Pod %s: %+v", name, p)
		}
		markReady(ctx, c, p)
	}
	waitFor(t, "a ready set", func() bool {
		s := getSet(t, ctx, c, "db")
		return s.Status.ReadyReplicas == 3 && s.Status.Replicas == 3 && s.Status.CurrentRevision != "" &&
			s.Status.CurrentRevision == s.Status.UpdateRevision && s.Status.CurrentReplicas == 3 && s.Status.ObservedGeneration == s.Generation
	})
	if got := claimNames(ctx, c); strings.Join(got, ",") != "data-db-0,data-db-1,data-db-2" {
		t.Fatalf("claims: %v", got)
	}
	pvc, _ := client.PersistentVolumeClaims(c, "default").Get(ctx, "data-db-0")
	if pvc.Labels["app"] != "db" || len(pvc.OwnerReferences) != 0 {
		t.Fatalf("claim of a Retain set: %+v", pvc.ObjectMeta)
	}

	updateSet(t, ctx, c, "db", func(s *appsv1.StatefulSet) { s.Spec.Replicas = ptr.To[int32](1) })
	waitFor(t, "the scale down", func() bool { return len(setPods(ctx, c, "db")) == 1 })
	if _, ok := setPods(ctx, c, "db")["db-0"]; !ok {
		t.Fatal("db-0 must stay")
	}
	waitFor(t, "the delete events", func() bool { return len(eventOrder(ctx, c, "db", "SuccessfulDelete")) == 2 })
	if got := eventOrder(ctx, c, "db", "SuccessfulDelete"); got[0] != "Delete Pod db-2 in StatefulSet db successful" ||
		got[1] != "Delete Pod db-1 in StatefulSet db successful" {
		t.Fatalf("deletion order: %v", got)
	}
	if got := eventOrder(ctx, c, "db", "SuccessfulCreate"); len(got) != 6 || got[0] != "Create Claim data-db-0 Pod db-0 in StatefulSet db success" ||
		got[1] != "Create Pod db-0 in StatefulSet db successful" {
		t.Fatalf("create events: %v", got)
	}
	if got := claimNames(ctx, c); len(got) != 3 {
		t.Fatalf("claims are retained: %v", got)
	}
}

func TestStatefulSetPartitionedRollingUpdateAndRollback(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startStatefulSetControllers(t, c)
	go readyAll(ctx, c)
	if _, err := client.StatefulSets(c, "default").Create(ctx, newStatefulSet("web", 3, "1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a ready set", func() bool { return getSet(t, ctx, c, "web").Status.ReadyReplicas == 3 })
	first := getSet(t, ctx, c, "web").Status.CurrentRevision

	updateSet(t, ctx, c, "web", func(s *appsv1.StatefulSet) {
		s.Spec.Template.Spec.Containers[0].Env[0].Value = "2"
		s.Spec.UpdateStrategy.RollingUpdate.Partition = ptr.To[int32](2)
	})
	var second string
	waitFor(t, "web-2 alone to be updated", func() bool {
		s := getSet(t, ctx, c, "web")
		second = s.Status.UpdateRevision
		pods := setPods(ctx, c, "web")
		return second != first && s.Status.UpdatedReplicas == 1 && s.Status.CurrentReplicas == 2 && s.Status.ReadyReplicas == 3 &&
			pods["web-2"] == second && pods["web-1"] == first && pods["web-0"] == first
	})
	if s := getSet(t, ctx, c, "web"); s.Status.CurrentRevision != first {
		t.Fatalf("the current revision moves only when every Pod is updated: %+v", s.Status)
	}

	updateSet(t, ctx, c, "web", func(s *appsv1.StatefulSet) { s.Spec.UpdateStrategy.RollingUpdate.Partition = ptr.To[int32](0) })
	waitFor(t, "every Pod to be updated", func() bool {
		s := getSet(t, ctx, c, "web")
		pods := setPods(ctx, c, "web")
		return s.Status.CurrentRevision == second && s.Status.UpdatedReplicas == 3 && s.Status.ReadyReplicas == 3 &&
			pods["web-0"] == second && pods["web-1"] == second
	})
	deletes := eventOrder(ctx, c, "web", "SuccessfulDelete")
	if strings.Join(deletes, "|") != "Delete Pod web-2 in StatefulSet web successful|Delete Pod web-1 in StatefulSet web successful|"+
		"Delete Pod web-0 in StatefulSet web successful" {
		t.Fatalf("update order: %v", deletes)
	}

	// Rolling back reuses the first revision, now the newest.
	updateSet(t, ctx, c, "web", func(s *appsv1.StatefulSet) { s.Spec.Template.Spec.Containers[0].Env[0].Value = "1" })
	waitFor(t, "the rollback", func() bool {
		s := getSet(t, ctx, c, "web")
		pods := setPods(ctx, c, "web")
		return s.Status.CurrentRevision == first && s.Status.UpdatedReplicas == 3 && pods["web-0"] == first && pods["web-2"] == first
	})
	list, _ := client.ControllerRevisions(c, "default").List(ctx, client.ListOptions{})
	revisions := map[string]int64{}
	for _, cr := range list.Items {
		revisions[cr.Name] = cr.Revision
		if cr.Labels[appsv1.ControllerRevisionHashLabel] == "" || !strings.HasPrefix(cr.Name, "web-") {
			t.Errorf("revision %s labels %v", cr.Name, cr.Labels)
		}
	}
	if len(revisions) != 2 || revisions[first] != 3 || revisions[second] != 2 {
		t.Fatalf("revisions: %v", revisions)
	}

	// OnDelete: a template change waits for Pods to be deleted.
	updateSet(t, ctx, c, "web", func(s *appsv1.StatefulSet) {
		s.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}
		s.Spec.Template.Spec.Containers[0].Env[0].Value = "3"
	})
	var third string
	waitFor(t, "the new update revision", func() bool {
		s := getSet(t, ctx, c, "web")
		third = s.Status.UpdateRevision
		return s.Status.ObservedGeneration == s.Generation && third != first && third != second
	})
	time.Sleep(500 * time.Millisecond)
	if pods := setPods(ctx, c, "web"); pods["web-1"] != first {
		t.Fatalf("OnDelete replaced a Pod: %v", pods)
	}
	if err := client.Pods(c, "default").Delete(ctx, "web-1", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "web-1 at the new revision", func() bool { return setPods(ctx, c, "web")["web-1"] == third })
	if pods := setPods(ctx, c, "web"); pods["web-0"] != first || pods["web-2"] != first {
		t.Fatalf("only the deleted Pod changes: %v", pods)
	}
}

func TestParallelStatefulSetDeletesClaimsByPolicy(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startStatefulSetControllers(t, c)
	s := newStatefulSet("cache", 3, "1")
	s.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
	s.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType, WhenScaled: appsv1.DeletePersistentVolumeClaimRetentionPolicyType}
	created, err := client.StatefulSets(c, "default").Create(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	// Parallel: all three at once, none of them ready.
	waitFor(t, "three Pods", func() bool { return len(setPods(ctx, c, "cache")) == 3 })
	pvc, err := client.PersistentVolumeClaims(c, "default").Get(ctx, "data-cache-2")
	if err != nil {
		t.Fatal(err)
	}
	if ref := metav1.ControllerOf(&pvc.ObjectMeta); ref == nil || ref.UID != created.UID {
		t.Fatalf("a claim deleted with its set is owned by it: %+v", pvc.OwnerReferences)
	}

	updateSet(t, ctx, c, "cache", func(s *appsv1.StatefulSet) { s.Spec.Replicas = ptr.To[int32](1) })
	waitFor(t, "the claims of removed Pods to go", func() bool {
		return strings.Join(claimNames(ctx, c), ",") == "data-cache-0" && len(setPods(ctx, c, "cache")) == 1
	})

	if err := client.StatefulSets(c, "default").Delete(ctx, "cache", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the last claim to go with the set", func() bool {
		return len(claimNames(ctx, c)) == 0 && len(setPods(ctx, c, "cache")) == 0
	})
}

func TestStatefulSetAdoptsOrphansAndReplacesFinishedPods(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startStatefulSetControllers(t, c)
	go readyAll(ctx, c)
	if _, err := client.StatefulSets(c, "default").Create(ctx, newStatefulSet("kv", 2, "1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a ready set", func() bool { return getSet(t, ctx, c, "kv").Status.ReadyReplicas == 2 })
	before, _ := client.Pods(c, "default").Get(ctx, "kv-0")

	orphan := metav1.DeletePropagationOrphan
	if err := client.StatefulSets(c, "default").Delete(ctx, "kv", &metav1.DeleteOptions{PropagationPolicy: &orphan}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the set to go and its Pods to be orphaned", func() bool {
		_, err := client.StatefulSets(c, "default").Get(ctx, "kv")
		p, perr := client.Pods(c, "default").Get(ctx, "kv-0")
		return client.IsNotFound(err) && perr == nil && metav1.ControllerOf(&p.ObjectMeta) == nil
	})
	created, err := client.StatefulSets(c, "default").Create(ctx, newStatefulSet("kv", 2, "1"))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the Pods to be adopted", func() bool {
		p, err := client.Pods(c, "default").Get(ctx, "kv-0")
		ref := metav1.ControllerOf(&p.ObjectMeta)
		return err == nil && ref != nil && ref.UID == created.UID && getSet(t, ctx, c, "kv").Status.ReadyReplicas == 2
	})
	if p, _ := client.Pods(c, "default").Get(ctx, "kv-0"); p.UID != before.UID {
		t.Fatal("the adopted Pod must be the same Pod")
	}

	// A Pod that ended is replaced by a new one of the same name.
	p, _ := client.Pods(c, "default").Get(ctx, "kv-1")
	p.Status.Phase = corev1.PodFailed
	if _, err := client.Pods(c, "default").UpdateStatus(ctx, p); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "kv-1 to come back", func() bool {
		n, err := client.Pods(c, "default").Get(ctx, "kv-1")
		return err == nil && n.UID != p.UID && n.Status.Phase == corev1.PodRunning
	})
}
