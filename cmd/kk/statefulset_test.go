package main

import (
	"context"
	"strings"
	"testing"

	"mini-k8s/pkg/api/resource"
	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/controller/history"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/test/integration/framework"
)

func TestStatefulSetRolloutStatus(t *testing.T) {
	set := func(replicas, partition int32, st appsv1.StatefulSetStatus) *appsv1.StatefulSet {
		s := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "web", Generation: 2}, Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(replicas), UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: "RollingUpdate",
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To(partition)}}}, Status: st}
		return s
	}
	for _, tc := range []struct {
		set  *appsv1.StatefulSet
		want string
		done bool
	}{
		{set(3, 0, appsv1.StatefulSetStatus{ObservedGeneration: 1}), "Waiting for statefulset spec update to be observed...", false},
		{set(3, 0, appsv1.StatefulSetStatus{ObservedGeneration: 2, ReadyReplicas: 2}), "Waiting for 1 pods to be ready...", false},
		{set(3, 2, appsv1.StatefulSetStatus{ObservedGeneration: 2, ReadyReplicas: 3}),
			"Waiting for partitioned roll out to finish: 0 out of 1 new pods have been updated...", false},
		{set(3, 2, appsv1.StatefulSetStatus{ObservedGeneration: 2, ReadyReplicas: 3, UpdatedReplicas: 1}),
			"partitioned roll out complete: 1 new pods have been updated...", true},
		{set(3, 0, appsv1.StatefulSetStatus{ObservedGeneration: 2, ReadyReplicas: 3, UpdatedReplicas: 3}),
			"partitioned roll out complete: 3 new pods have been updated...", true},
	} {
		got, done, err := StatefulSetRolloutStatus(tc.set)
		if err != nil || got != tc.want || done != tc.done {
			t.Errorf("%+v: %q %v %v, want %q %v", tc.set.Status, got, done, err, tc.want, tc.done)
		}
	}
	onDelete := set(1, 0, appsv1.StatefulSetStatus{})
	onDelete.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: "OnDelete"}
	if _, _, err := StatefulSetRolloutStatus(onDelete); err == nil || err.Error() != "rollout status is only available for RollingUpdate strategy type" {
		t.Fatalf("OnDelete: %v", err)
	}
}

func TestStatefulSetCommands(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := context.Background()
	tpl := func(v string) corev1.PodTemplateSpec {
		return corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img:" + v}}}}
	}
	s, err := client.StatefulSets(c, "default").Create(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](3), ServiceName: "web",
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, Template: tpl("2"),
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"},
				Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []string{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResources{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := kk(t, c, "", "get", "sts", "-o", "wide")
	if err != nil || !strings.Contains(strings.Join(strings.Fields(out), " "), "NAME READY AGE CONTAINERS IMAGES web 0/3") {
		t.Fatalf("kk get sts: %v\n%s", err, out)
	}
	if out, err := kk(t, c, "", "scale", "statefulset", "web", "--replicas", "2"); err != nil || out != "statefulset.apps/web scaled\n" {
		t.Fatalf("scale: %q %v", out, err)
	}
	out, err = kk(t, c, "", "describe", "sts", "web")
	for _, want := range []string{"Replicas:", "2 desired | 0 total", "Partition:", "Volume Claims:", "data (RWO, 1Gi)",
		"whenDeleted: Retain, whenScaled: Retain"} {
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("describe lacks %q: %v\n%s", want, err, out)
		}
	}

	// Two revisions, as the controller would have made them.
	for i, v := range []string{"1", "2"} {
		pt := tpl(v)
		_, err := client.ControllerRevisions(c, "default").Create(ctx, &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{
			Name: "web-r" + v, OwnerReferences: []metav1.OwnerReference{metav1.NewControllerRef(s)}},
			Data: history.RevisionData(&pt), Revision: int64(i + 1)})
		if err != nil {
			t.Fatal(err)
		}
	}
	out, err = kk(t, c, "", "rollout", "history", "sts/web")
	if err != nil || out != "statefulset.apps/web \nREVISION   CHANGE-CAUSE\n1          <none>\n2          <none>\n\n" {
		t.Fatalf("history: %v\n%q", err, out)
	}
	if out, err := kk(t, c, "", "rollout", "history", "statefulset", "web", "--revision", "1"); err != nil || !strings.Contains(out, "image: img:1") {
		t.Fatalf("history --revision: %v\n%s", err, out)
	}
	if out, err := kk(t, c, "", "rollout", "undo", "statefulset", "web"); err != nil || out != "statefulset.apps/web rolled back\n" {
		t.Fatalf("undo: %q %v", out, err)
	}
	if got, _ := client.StatefulSets(c, "default").Get(ctx, "web"); got.Spec.Template.Spec.Containers[0].Image != "img:1" {
		t.Fatalf("undo wrote %s", got.Spec.Template.Spec.Containers[0].Image)
	}
	if out, err := kk(t, c, "", "rollout", "restart", "statefulset/web"); err != nil || out != "statefulset.apps/web restarted\n" {
		t.Fatalf("restart: %q %v", out, err)
	}
	if got, _ := client.StatefulSets(c, "default").Get(ctx, "web"); got.Spec.Template.Annotations[appsv1.RestartedAtAnnotation] == "" {
		t.Fatal("restart sets restartedAt")
	}
	if _, err := kk(t, c, "", "rollout", "pause", "statefulset", "web"); err == nil ||
		err.Error() != `statefulsets.apps "web" pausing is not supported` {
		t.Fatalf("pause: %v", err)
	}
}
