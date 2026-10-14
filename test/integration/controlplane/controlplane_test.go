// Package controlplane runs the API server, controllers and scheduler together
// against a real etcd, with a fake kubelet, to test their interplay.
package controlplane

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/controller/deployment"
	"mini-k8s/pkg/controller/garbagecollector"
	"mini-k8s/pkg/controller/namespace"
	"mini-k8s/pkg/controller/replicaset"
	"mini-k8s/pkg/scheduler"
	"mini-k8s/pkg/util/intstr"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/test/integration/framework"
)

// startControlPlane runs the controllers, the scheduler and a fake kubelet
// that reports bound Pods Running/Ready and completes graceful deletions.
func startControlPlane(t *testing.T, c *client.Client, nodes ...string) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, n := range nodes {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n}}
		created, err := client.Nodes(c).Create(ctx, node)
		if err != nil {
			t.Fatal(err)
		}
		go heartbeat(ctx, c, created.Name)
	}
	rec := record.New(c, "test", "")
	go rec.Run(ctx)
	pods := informers.New(client.Pods(c, ""), client.ListOptions{}, 0)
	nodeInf := informers.New(client.Nodes(c), client.ListOptions{}, 0)
	deps := informers.New(client.Deployments(c, ""), client.ListOptions{}, 0)
	rsets := informers.New(client.ReplicaSets(c, ""), client.ListOptions{}, 0)
	nss := informers.New(client.Namespaces(c), client.ListOptions{}, 0)
	dc := deployment.NewDeploymentController(c, deps, rsets, pods, rec)
	rc := replicaset.NewController(c, rsets, pods, rec)
	nc := namespace.NewNamespaceController(c, nss)
	sched := scheduler.New(c, pods, nodeInf, rec)
	gc := garbagecollector.NewGarbageCollector(c, garbagecollector.TrackedResources())
	for _, run := range []func(context.Context){pods.Run, nodeInf.Run, deps.Run, rsets.Run, nss.Run} {
		go run(ctx)
	}
	informers.WaitForSync(ctx, pods.HasSynced, nodeInf.HasSynced, deps.HasSynced, rsets.HasSynced, nss.HasSynced)
	go dc.Run(ctx, 2)
	go rc.Run(ctx, 2)
	go nc.Run(ctx, 1)
	go sched.Run(ctx)
	go gc.Run(ctx)
	go fakeKubelet(ctx, c)
	return ctx
}

func heartbeat(ctx context.Context, c *client.Client, name string) {
	for ctx.Err() == nil {
		if n, err := client.Nodes(c).Get(ctx, name); err == nil {
			now := metav1.Now()
			n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: now, LastTransitionTime: now}}
			_, _ = client.Nodes(c).UpdateStatus(ctx, n)
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func fakeKubelet(ctx context.Context, c *client.Client) {
	for ctx.Err() == nil {
		list, err := client.Pods(c, "").List(ctx, client.ListOptions{})
		if err == nil {
			for i := range list.Items {
				p := &list.Items[i]
				switch {
				case p.Spec.NodeName == "":
				case p.DeletionTimestamp != nil:
					_ = client.Pods(c, p.Namespace).Delete(ctx, p.Name, &metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
				case p.Status.Phase != corev1.PodRunning:
					p.Status.Phase = corev1.PodRunning
					p.Status.SetCondition(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue})
					_, _ = client.Pods(c, p.Namespace).UpdateStatus(ctx, p)
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func eventually(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		ok, detail := cond()
		if ok {
			return
		}
		last = detail
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %s", what, last)
}

func newDeployment(name string, replicas int32, image string) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: appsv1.DeploymentSpec{
		Replicas: ptr.To[int32](replicas), Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}}},
	}}
}

func livePods(t *testing.T, c *client.Client, ns, selector string) []corev1.Pod {
	t.Helper()
	list, err := client.Pods(c, ns).List(context.Background(), client.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Fatal(err)
	}
	var out []corev1.Pod
	for _, p := range list.Items {
		if p.DeletionTimestamp == nil {
			out = append(out, p)
		}
	}
	return out
}

func TestDeploymentScalesSpreadsAndReportsStatus(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startControlPlane(t, c, "worker-1", "worker-2")
	deps := client.Deployments(c, "default")
	d, err := deps.Create(ctx, newDeployment("web", 4, "img:1"))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "4 ready replicas spread 2/2", func() (bool, string) {
		pods := livePods(t, c, "default", "app=web")
		perNode := map[string]int{}
		for _, p := range pods {
			ref := metav1.ControllerOf(&p.ObjectMeta)
			if ref == nil || ref.Kind != "ReplicaSet" || p.Labels[appsv1.DefaultDeploymentUniqueLabelKey] == "" {
				return false, fmt.Sprintf("bad owner/labels on %s", p.Name)
			}
			rs, err := client.ReplicaSets(c, "default").Get(ctx, ref.Name)
			if err != nil || metav1.ControllerOf(&rs.ObjectMeta).UID != d.UID {
				return false, fmt.Sprintf("ReplicaSet %s not controlled by the Deployment: %v", ref.Name, err)
			}
			perNode[p.Spec.NodeName]++
		}
		cur, _ := deps.Get(ctx, "web")
		return len(pods) == 4 && perNode["worker-1"] == 2 && perNode["worker-2"] == 2 &&
				cur.Status.ReadyReplicas == 4 && cur.Status.ObservedGeneration == cur.Generation,
			fmt.Sprintf("pods=%d perNode=%v status=%+v", len(pods), perNode, cur.Status)
	})
	if _, err := deps.Patch(ctx, "web", map[string]any{"spec": map[string]any{"replicas": 1}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "scale down to 1", func() (bool, string) {
		n := len(livePods(t, c, "default", "app=web"))
		return n == 1, fmt.Sprint(n)
	})
}

func TestDeploymentTemplateChangeReplacesPods(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startControlPlane(t, c, "worker-1")
	deps := client.Deployments(c, "default")
	if _, err := deps.Create(ctx, newDeployment("app", 2, "img:1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "2 pods", func() (bool, string) { return len(livePods(t, c, "default", "app=app")) == 2, "" })
	old := livePods(t, c, "default", "app=app")[0].Labels[appsv1.DefaultDeploymentUniqueLabelKey]
	if _, err := deps.Patch(ctx, "app", map[string]any{"spec": map[string]any{"template": map[string]any{
		"spec": map[string]any{"containers": []any{map[string]any{"name": "c", "image": "img:2"}}}}}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "all pods on the new template", func() (bool, string) {
		pods := livePods(t, c, "default", "app=app")
		for _, p := range pods {
			if p.Labels[appsv1.DefaultDeploymentUniqueLabelKey] == old || p.Spec.Containers[0].Image != "img:2" {
				return false, p.Name
			}
		}
		return len(pods) == 2, fmt.Sprint(len(pods))
	})
}

func TestGarbageCollectionPolicies(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startControlPlane(t, c, "worker-1")
	deps := client.Deployments(c, "default")
	for _, name := range []string{"bg", "orphan", "fg"} {
		if _, err := deps.Create(ctx, newDeployment(name, 2, "img")); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"bg", "orphan", "fg"} {
		sel := "app=" + name
		eventually(t, name+" pods", func() (bool, string) { return len(livePods(t, c, "default", sel)) == 2, "" })
	}

	if err := deps.Delete(ctx, "bg", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "background deletion removes dependents", func() (bool, string) {
		return len(livePods(t, c, "default", "app=bg")) == 0, ""
	})

	orphan := metav1.DeletePropagationOrphan
	if err := deps.Delete(ctx, "orphan", &metav1.DeleteOptions{PropagationPolicy: &orphan}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the orphaned ReplicaSet keeps its Pods running without an owner", func() (bool, string) {
		_, err := deps.Get(ctx, "orphan")
		rsets, _ := client.ReplicaSets(c, "default").List(ctx, client.ListOptions{LabelSelector: "app=orphan"})
		if len(rsets.Items) != 1 || len(rsets.Items[0].OwnerReferences) != 0 {
			return false, fmt.Sprintf("replica sets: %+v", rsets.Items)
		}
		pods := livePods(t, c, "default", "app=orphan")
		for _, p := range pods {
			if ref := metav1.ControllerOf(&p.ObjectMeta); ref == nil || ref.UID != rsets.Items[0].UID {
				return false, "Pods must stay with their ReplicaSet"
			}
		}
		return client.IsNotFound(err) && len(pods) == 2, fmt.Sprintf("err=%v pods=%d", err, len(pods))
	})

	fg := metav1.DeletePropagationForeground
	if err := deps.Delete(ctx, "fg", &metav1.DeleteOptions{PropagationPolicy: &fg}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "foreground deletion removes pods, then the owner", func() (bool, string) {
		_, err := deps.Get(ctx, "fg")
		list, _ := client.Pods(c, "default").List(ctx, client.ListOptions{LabelSelector: "app=fg"})
		return client.IsNotFound(err) && len(list.Items) == 0, fmt.Sprintf("err=%v pods=%d", err, len(list.Items))
	})
}

func TestNamespaceDeletionRemovesContents(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startControlPlane(t, c, "worker-1")
	if _, err := client.Namespaces(c).Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Deployments(c, "team").Create(ctx, newDeployment("web", 2, "img")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "pods in team", func() (bool, string) { return len(livePods(t, c, "team", "")) == 2, "" })
	if err := client.Namespaces(c).Delete(ctx, "team", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "namespace and contents gone", func() (bool, string) {
		_, err := client.Namespaces(c).Get(ctx, "team")
		return client.IsNotFound(err), fmt.Sprint(err)
	})
	list, _ := client.Pods(c, "team").List(ctx, client.ListOptions{})
	if len(list.Items) != 0 {
		t.Fatalf("pods left behind: %d", len(list.Items))
	}
}

func TestSchedulerReportsUnschedulable(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startControlPlane(t, c) // no nodes
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "lonely"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}}}
	if _, err := client.Pods(c, "default").Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, "PodScheduled=False", func() (bool, string) {
		got, _ := client.Pods(c, "default").Get(ctx, "lonely")
		cond := got.Status.Condition(corev1.PodScheduled)
		return cond != nil && cond.Status == corev1.ConditionFalse && cond.Reason == "Unschedulable", fmt.Sprintf("%+v", got.Status)
	})
	eventually(t, "FailedScheduling event", func() (bool, string) {
		evs, _ := client.Events(c, "default").List(ctx, client.ListOptions{FieldSelector: "involvedObject.name=lonely,reason=FailedScheduling"})
		return len(evs.Items) == 1 && evs.Items[0].Type == corev1.EventTypeWarning, ""
	})
}

func TestNamespaceDeletedWithOrphanPolicyIsReleased(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startControlPlane(t, c, "worker-1")
	if _, err := client.Namespaces(c).Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orphaned"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Deployments(c, "orphaned").Create(ctx, newDeployment("web", 1, "img")); err != nil {
		t.Fatal(err)
	}
	orphan := metav1.DeletePropagationOrphan
	if err := client.Namespaces(c).Delete(ctx, "orphaned", &metav1.DeleteOptions{PropagationPolicy: &orphan}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "namespace deleted despite the orphan finalizer", func() (bool, string) {
		ns, err := client.Namespaces(c).Get(ctx, "orphaned")
		if err == nil {
			return false, fmt.Sprintf("finalizers=%v phase=%s", ns.Finalizers, ns.Status.Phase)
		}
		return client.IsNotFound(err), fmt.Sprint(err)
	})
}

func replicaSetsOf(t *testing.T, c *client.Client, app string) []appsv1.ReplicaSet {
	t.Helper()
	list, err := client.ReplicaSets(c, "default").List(context.Background(), client.ListOptions{LabelSelector: "app=" + app})
	if err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func setImage(t *testing.T, c *client.Client, name, image string) {
	t.Helper()
	_, err := client.Deployments(c, "default").Patch(context.Background(), name, map[string]any{"spec": map[string]any{"template": map[string]any{
		"spec": map[string]any{"containers": []any{map[string]any{"name": "c", "image": image}}}}}})
	if err != nil {
		t.Fatal(err)
	}
}

// rolledOut reports whether every live Pod runs image and the Deployment
// reports a completed rollout.
func rolledOut(t *testing.T, c *client.Client, name, image string, replicas int) (bool, string) {
	pods := livePods(t, c, "default", "app="+name)
	for _, p := range pods {
		if p.Spec.Containers[0].Image != image {
			return false, p.Name + " runs " + p.Spec.Containers[0].Image
		}
	}
	d, err := client.Deployments(c, "default").Get(context.Background(), name)
	if err != nil {
		return false, err.Error()
	}
	cond := d.Status.Condition(appsv1.DeploymentProgressing)
	return len(pods) == replicas && d.Status.UpdatedReplicas == int32(replicas) && d.Status.AvailableReplicas == int32(replicas) &&
			cond != nil && cond.Reason == "NewReplicaSetAvailable",
		fmt.Sprintf("pods=%d status=%+v", len(pods), d.Status)
}

func TestRollingUpdateStaysWithinSurgeAndAvailability(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startControlPlane(t, c, "worker-1", "worker-2")
	d := newDeployment("roll", 4, "img:1")
	zero, one := intstr.FromInt(0), intstr.FromInt(1)
	d.Spec.Strategy = appsv1.DeploymentStrategy{Type: "RollingUpdate", RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: &zero, MaxSurge: &one}}
	if _, err := client.Deployments(c, "default").Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first rollout", func() (bool, string) { return rolledOut(t, c, "roll", "img:1", 4) })
	setImage(t, c, "roll", "img:2")
	deadline := time.Now().Add(15 * time.Second)
	for {
		pods := livePods(t, c, "default", "app=roll")
		ready := 0
		for _, p := range pods {
			if corev1.IsPodReady(&p) {
				ready++
			}
		}
		if len(pods) > 5 {
			t.Fatalf("maxSurge 1 exceeded: %d Pods", len(pods))
		}
		if ready < 4 && len(pods) <= 4 {
			t.Fatalf("maxUnavailable 0 violated: %d ready of %d", ready, len(pods))
		}
		if ok, _ := rolledOut(t, c, "roll", "img:2", 4); ok {
			break
		}
		if time.Now().After(deadline) {
			_, detail := rolledOut(t, c, "roll", "img:2", 4)
			t.Fatalf("rollout did not finish: %s", detail)
		}
		time.Sleep(20 * time.Millisecond)
	}
	rsets := replicaSetsOf(t, c, "roll")
	if len(rsets) != 2 {
		t.Fatalf("want the old and the new ReplicaSet, got %d", len(rsets))
	}
	for _, rs := range rsets {
		want := map[string]string{"img:1": "1", "img:2": "2"}[rs.Spec.Template.Spec.Containers[0].Image]
		if rs.Annotations[appsv1.RevisionAnnotation] != want {
			t.Fatalf("revision of %s: %q, want %q", rs.Name, rs.Annotations[appsv1.RevisionAnnotation], want)
		}
	}
	cur, _ := client.Deployments(c, "default").Get(ctx, "roll")
	if cur.Annotations[appsv1.RevisionAnnotation] != "2" {
		t.Fatalf("deployment revision: %v", cur.Annotations)
	}
	evs, _ := client.Events(c, "default").List(ctx, client.ListOptions{FieldSelector: "involvedObject.name=roll,reason=ScalingReplicaSet"})
	if len(evs.Items) == 0 || !strings.HasPrefix(evs.Items[0].Message, "Scaled up replica set roll-") {
		t.Fatalf("ScalingReplicaSet events: %+v", evs.Items)
	}
}

func TestRecreateStopsOldPodsFirst(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startControlPlane(t, c, "worker-1")
	d := newDeployment("rec", 2, "img:1")
	d.Spec.Strategy.Type = appsv1.RecreateDeploymentStrategyType
	if _, err := client.Deployments(c, "default").Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first rollout", func() (bool, string) { return rolledOut(t, c, "rec", "img:1", 2) })
	setImage(t, c, "rec", "img:2")
	deadline := time.Now().Add(15 * time.Second)
	for {
		list, _ := client.Pods(c, "default").List(ctx, client.ListOptions{LabelSelector: "app=rec"})
		images := map[string]bool{}
		for _, p := range list.Items {
			images[p.Spec.Containers[0].Image] = true
		}
		if images["img:1"] && images["img:2"] {
			t.Fatal("Recreate must not run old and new Pods at the same time")
		}
		if ok, _ := rolledOut(t, c, "rec", "img:2", 2); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("rollout did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRollbackReusesTheOldReplicaSetAndHistoryIsBounded(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startControlPlane(t, c, "worker-1")
	d := newDeployment("hist", 1, "img:1")
	d.Spec.RevisionHistoryLimit = ptr.To[int32](1)
	if _, err := client.Deployments(c, "default").Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	eventually(t, "v1", func() (bool, string) { return rolledOut(t, c, "hist", "img:1", 1) })
	setImage(t, c, "hist", "img:2")
	eventually(t, "v2", func() (bool, string) { return rolledOut(t, c, "hist", "img:2", 1) })
	setImage(t, c, "hist", "img:1") // a rollback to revision 1
	eventually(t, "back on v1 with the old ReplicaSet as revision 3", func() (bool, string) {
		ok, detail := rolledOut(t, c, "hist", "img:1", 1)
		rsets := replicaSetsOf(t, c, "hist")
		for _, rs := range rsets {
			if rs.Spec.Template.Spec.Containers[0].Image == "img:1" && rs.Annotations[appsv1.RevisionAnnotation] != "3" {
				return false, "revision " + rs.Annotations[appsv1.RevisionAnnotation]
			}
		}
		return ok && len(rsets) == 2, fmt.Sprintf("%s rsets=%d", detail, len(rsets))
	})
	setImage(t, c, "hist", "img:3")
	eventually(t, "v3 with one old ReplicaSet kept", func() (bool, string) {
		ok, detail := rolledOut(t, c, "hist", "img:3", 1)
		n := len(replicaSetsOf(t, c, "hist"))
		return ok && n == 2, fmt.Sprintf("%s rsets=%d", detail, n)
	})
}

func TestPausedDeploymentDoesNotRollOut(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startControlPlane(t, c, "worker-1")
	deps := client.Deployments(c, "default")
	if _, err := deps.Create(ctx, newDeployment("p", 2, "img:1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "v1", func() (bool, string) { return rolledOut(t, c, "p", "img:1", 2) })
	if _, err := deps.Patch(ctx, "p", map[string]any{"spec": map[string]any{"paused": true}}); err != nil {
		t.Fatal(err)
	}
	setImage(t, c, "p", "img:2")
	if _, err := deps.Patch(ctx, "p", map[string]any{"spec": map[string]any{"replicas": 3}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "scaled but not rolled out while paused", func() (bool, string) {
		d, _ := deps.Get(ctx, "p")
		cond := d.Status.Condition(appsv1.DeploymentProgressing)
		pods := livePods(t, c, "default", "app=p")
		return len(pods) == 3 && len(replicaSetsOf(t, c, "p")) == 1 && cond != nil && cond.Reason == "DeploymentPaused",
			fmt.Sprintf("pods=%d status=%+v", len(pods), d.Status)
	})
	if _, err := deps.Patch(ctx, "p", map[string]any{"spec": map[string]any{"paused": false}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "rolled out after resuming", func() (bool, string) { return rolledOut(t, c, "p", "img:2", 3) })
}

func TestOrphanedReplicaSetIsAdoptedByARecreatedDeployment(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startControlPlane(t, c, "worker-1")
	deps := client.Deployments(c, "default")
	if _, err := deps.Create(ctx, newDeployment("again", 2, "img:1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first rollout", func() (bool, string) { return rolledOut(t, c, "again", "img:1", 2) })
	orphan := metav1.DeletePropagationOrphan
	if err := deps.Delete(ctx, "again", &metav1.DeleteOptions{PropagationPolicy: &orphan}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the ReplicaSet orphaned", func() (bool, string) {
		_, err := deps.Get(ctx, "again")
		rsets := replicaSetsOf(t, c, "again")
		return client.IsNotFound(err) && len(rsets) == 1 && len(rsets[0].OwnerReferences) == 0, fmt.Sprint(err)
	})
	d, err := deps.Create(ctx, newDeployment("again", 3, "img:1"))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the recreated Deployment adopts and scales the ReplicaSet", func() (bool, string) {
		ok, detail := rolledOut(t, c, "again", "img:1", 3)
		rsets := replicaSetsOf(t, c, "again")
		if len(rsets) != 1 {
			return false, fmt.Sprintf("%d replica sets", len(rsets))
		}
		ref := metav1.ControllerOf(&rsets[0].ObjectMeta)
		return ok && ref != nil && ref.UID == d.UID, detail
	})
}

func TestPodsOwnedDirectlyByADeploymentAreReplaced(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startControlPlane(t, c, "worker-1")
	d, err := client.Deployments(c, "default").Create(ctx, newDeployment("legacy", 1, "img:1"))
	if err != nil {
		t.Fatal(err)
	}
	// A Pod as created by the controller before Deployments used ReplicaSets.
	legacy := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "legacy-old", Labels: map[string]string{"app": "legacy"},
		OwnerReferences: []metav1.OwnerReference{metav1.NewControllerRef(d)}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img:0"}}}}
	if _, err := client.Pods(c, "default").Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the legacy Pod deleted once the ReplicaSet's Pods are available", func() (bool, string) {
		ok, detail := rolledOut(t, c, "legacy", "img:1", 1)
		_, err := client.Pods(c, "default").Get(ctx, "legacy-old")
		return ok && client.IsNotFound(err), detail
	})
}
