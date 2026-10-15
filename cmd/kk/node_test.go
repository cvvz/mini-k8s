package main

import (
	"context"
	"strings"
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/integration/framework"
)

func TestCordonAndTaint(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := context.Background()
	if _, err := client.Nodes(c).Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"cordon", "n1"}, "node/n1 cordoned\n"},
		{[]string{"cordon", "n1"}, "node/n1 already cordoned\n"},
		{[]string{"uncordon", "n1"}, "node/n1 uncordoned\n"},
		{[]string{"taint", "nodes", "n1", "dedicated=gpu:NoExecute"}, "node/n1 tainted\n"},
		{[]string{"taint", "node/n1", "dedicated=cpu:NoExecute", "--overwrite"}, "node/n1 tainted\n"},
		{[]string{"taint", "nodes", "n1", "dedicated:NoExecute-", "spare=1:NoSchedule"}, "node/n1 modified\n"},
		{[]string{"taint", "nodes", "n1", "spare-"}, "node/n1 untainted\n"},
	} {
		out, err := kk(t, c, "", tc.args...)
		if err != nil || out != tc.want {
			t.Fatalf("kk %v: %q %v, want %q", tc.args, out, err, tc.want)
		}
		if tc.args[0] == "taint" && strings.Contains(strings.Join(tc.args, " "), "overwrite") {
			n, _ := client.Nodes(c).Get(ctx, "n1")
			if len(n.Spec.Taints) != 1 || n.Spec.Taints[0].Value != "cpu" || n.Spec.Taints[0].TimeAdded == nil {
				t.Fatalf("NoExecute taints carry the time they were added: %+v", n.Spec.Taints)
			}
		}
	}
	if _, err := kk(t, c, "", "taint", "nodes", "n1", "a=b:NoSchedule"); err != nil {
		t.Fatal(err)
	}
	if _, err := kk(t, c, "", "taint", "nodes", "n1", "a=c:NoSchedule"); err == nil ||
		err.Error() != "node n1 already has a taint(s) with same effect(s) and --overwrite is false" {
		t.Fatalf("duplicate taint: %v", err)
	}
	if _, err := kk(t, c, "", "taint", "nodes", "n1", "missing-"); err == nil || !strings.Contains(err.Error(), `taint "missing" not found`) {
		t.Fatalf("missing taint: %v", err)
	}
}

func TestDrainEvictsManagedPodsAndRefusesOthers(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := context.Background()
	if _, err := client.Nodes(c).Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}); err != nil {
		t.Fatal(err)
	}
	pod := func(name string, mutate func(*corev1.Pod)) {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: "c", Image: "img"}}}}
		if mutate != nil {
			mutate(p)
		}
		if _, err := client.Pods(c, "default").Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	owned := func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web", UID: "u1", Controller: ptrTrue()}}
	}
	pod("web-1", owned)
	pod("cache", func(p *corev1.Pod) {
		owned(p)
		p.Spec.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	})
	pod("loner", nil)
	pod("static-n1", func(p *corev1.Pod) { p.Annotations = map[string]string{corev1.AnnotationConfigMirror: "hash"} })

	_, err := kk(t, c, "", "drain", "n1", "--grace-period", "0")
	want := `unable to drain node "n1" due to error: cannot delete Pods declare no controller (use --force to override): default/loner, ` +
		"cannot delete Pods with local storage (use --delete-emptydir-data to override): default/cache"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v\nwant %s", err, want)
	}
	if n, _ := client.Nodes(c).Get(ctx, "n1"); !n.Spec.Unschedulable {
		t.Fatal("drain cordons first")
	}
	out, err := kk(t, c, "", "drain", "n1", "--force", "--delete-emptydir-data", "--grace-period", "0", "--timeout", "30s")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"node/n1 already cordoned", "evicting pod default/web-1", "evicting pod default/loner",
		"pod/cache evicted", "node/n1 drained"} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("missing %q in\n%s", line, out)
		}
	}
	list, _ := client.Pods(c, "default").List(ctx, client.ListOptions{})
	if len(list.Items) != 1 || list.Items[0].Name != "static-n1" {
		t.Fatalf("only the mirror Pod remains: %+v", list.Items)
	}
}

func ptrTrue() *bool { b := true; return &b }

func TestLabel(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := context.Background()
	if _, err := client.Nodes(c).Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"label", "nodes", "n1", "disk=ssd", "zone=a"}, "node/n1 labeled\n"},
		{[]string{"label", "node/n1", "disk=ssd"}, "node/n1 not labeled\n"},
		{[]string{"label", "nodes", "n1", "disk=nvme", "--overwrite"}, "node/n1 labeled\n"},
		{[]string{"label", "nodes", "n1", "zone-"}, "node/n1 unlabeled\n"},
	} {
		if out, err := kk(t, c, "", tc.args...); err != nil || out != tc.want {
			t.Fatalf("kk %v: %q %v, want %q", tc.args, out, err, tc.want)
		}
	}
	if _, err := kk(t, c, "", "label", "nodes", "n1", "disk=hdd"); err == nil ||
		err.Error() != "'disk' already has a value (nvme), and --overwrite is false" {
		t.Fatalf("conflict: %v", err)
	}
	n, _ := client.Nodes(c).Get(ctx, "n1")
	if n.Labels["disk"] != "nvme" || n.Labels["zone"] != "" {
		t.Fatalf("labels: %v", n.Labels)
	}
}

func TestLabelWithEmptyValue(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := context.Background()
	if _, err := client.Nodes(c).Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}); err != nil {
		t.Fatal(err)
	}
	if out, err := kk(t, c, "", "label", "nodes", "n1", "role="); err != nil || out != "node/n1 labeled\n" {
		t.Fatalf("%q %v", out, err)
	}
	n, err := client.Nodes(c).Get(ctx, "n1")
	if value, ok := n.Labels["role"]; err != nil || !ok || value != "" {
		t.Fatalf("the empty-valued label exists: %v %v", n.Labels, err)
	}
}
