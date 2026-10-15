package kubelet

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/kubelet/kuberuntime"
	"mini-k8s/test/integration/framework"
)

const apiserverManifest = `apiVersion: v1
kind: Pod
metadata:
  name: kube-apiserver
  namespace: kube-system
  labels:
    component: kube-apiserver
spec:
  hostNetwork: true
  containers:
  - name: kube-apiserver
    image: mini-k8s:local
    imagePullPolicy: Never
    command: ["/mini", "kube-apiserver"]
`

func writeManifest(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadStaticPods(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "kube-apiserver.yaml", apiserverManifest)
	writeManifest(t, dir, "default-ns.yaml", "apiVersion: v1\nkind: Pod\nmetadata: {name: web}\nspec:\n  containers: [{name: web, image: mini-k8s:local}]\n")
	writeManifest(t, dir, "broken.yaml", "apiVersion: v1\nkind: Pod\nmetadata: {name: bad}\nspec: {containers: []}\n")
	writeManifest(t, dir, "notes.txt", "not a manifest")
	writeManifest(t, dir, ".hidden.yaml", "garbage: [")
	pods, errs := LoadStaticPods(dir, "control-plane")
	if len(pods) != 2 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "broken.yaml") {
		t.Fatalf("pods=%d errs=%v", len(pods), errs)
	}
	var api *corev1.Pod
	for _, p := range pods {
		if p.Name == "kube-apiserver-control-plane" {
			api = p
		} else if p.Name != "web-control-plane" || p.Namespace != "default" {
			t.Fatalf("unexpected Pod %s/%s", p.Namespace, p.Name)
		}
	}
	if api == nil || api.Namespace != "kube-system" || api.Spec.NodeName != "control-plane" || api.Spec.RestartPolicy != "Always" ||
		api.Annotations[corev1.AnnotationConfigSource] != "file" || api.Annotations[corev1.AnnotationConfigHash] != api.UID || len(api.UID) != 36 {
		t.Fatalf("static Pod: %+v", api)
	}
	again, _ := LoadStaticPods(dir, "control-plane")
	if _, ok := again[api.UID]; !ok {
		t.Fatal("the UID must be stable for an unchanged manifest")
	}
	other, _ := LoadStaticPods(dir, "worker-1")
	if _, ok := other[api.UID]; ok {
		t.Fatal("the UID must depend on the node")
	}
	writeManifest(t, dir, "kube-apiserver.yaml", apiserverManifest+"    args: [\"--v=2\"]\n")
	changed, _ := LoadStaticPods(dir, "control-plane")
	if _, ok := changed[api.UID]; ok {
		t.Fatal("an edited manifest must be a new Pod")
	}
	if pods, errs := LoadStaticPods(filepath.Join(dir, "missing"), "n"); pods == nil || len(pods) != 0 || len(errs) != 0 {
		t.Fatal("a missing directory has no static Pods")
	}
	if pods, errs := LoadStaticPods(filepath.Join(dir, "default-ns.yaml"), "n"); pods != nil || len(errs) != 1 {
		t.Fatal("an unreadable directory must be reported, not treated as empty")
	}

	// A copy of a manifest (say, a backup) must not run the Pod twice.
	writeManifest(t, dir, "kube-apiserver.yaml", apiserverManifest)
	writeManifest(t, dir, "kube-apiserver-backup.yaml", apiserverManifest+"    args: [\"--old\"]\n")
	pods, errs = LoadStaticPods(dir, "control-plane")
	count := 0
	for _, p := range pods {
		if p.Name == "kube-apiserver-control-plane" {
			count++
		}
	}
	if count != 1 || len(errs) != 2 || !strings.Contains(errs[0].Error()+errs[1].Error(), "is already defined in kube-apiserver-backup.yaml") {
		t.Fatalf("duplicate names: %d Pods, errors %v", count, errs)
	}
}

// fakeRuntime reports every Pod as running and records kills.
type fakeRuntime struct {
	mu     sync.Mutex
	killed []string
}

func (f *fakeRuntime) SyncPod(_ context.Context, pod *corev1.Pod) corev1.PodStatus {
	st := corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: pod.Status.QOSClass, StartTime: pod.Status.StartTime}
	for _, c := range pod.Spec.Containers {
		st.ContainerStatuses = append(st.ContainerStatuses, corev1.ContainerStatus{Name: c.Name, Image: c.Image, Ready: true,
			ContainerID: "containerd://" + c.Name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
	}
	st.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	return st
}

func (f *fakeRuntime) KillPod(_ context.Context, pod *corev1.Pod, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, pod.Name)
	return nil
}

func (f *fakeRuntime) kills() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.killed...)
}

func (f *fakeRuntime) Cleanup(context.Context, func() map[string]bool) error { return nil }
func (f *fakeRuntime) Logs(context.Context, *corev1.Pod, string, kuberuntime.LogOptions, io.Writer) error {
	return nil
}
func (f *fakeRuntime) Exec(context.Context, *corev1.Pod, string, []string, time.Duration) (*corev1.PodExecResult, error) {
	return &corev1.PodExecResult{}, nil
}
func (f *fakeRuntime) StopContainer(context.Context, *corev1.Pod, string, int64) error { return nil }
func (f *fakeRuntime) Version(context.Context) (string, error)                         { return "fake://1", nil }
func (f *fakeRuntime) Status(context.Context, string) kuberuntime.Readiness {
	return kuberuntime.Readiness{Ready: true}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestStaticPodsAndMirrorPods(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	writeManifest(t, dir, "kube-apiserver.yaml", apiserverManifest)
	rt := &fakeRuntime{}
	rec := record.New(c, "kubelet", "control-plane")
	go rec.Run(ctx)
	k := New(c, rt, rec, Config{NodeName: "control-plane", Address: "10.0.0.1", CNIConfDir: t.TempDir(),
		StaticPodPath: dir, Role: "control-plane"})
	go k.Run(ctx)
	pods := client.Pods(c, "kube-system")
	mirror := func() (*corev1.Pod, error) { return pods.Get(ctx, "kube-apiserver-control-plane") }

	var first *corev1.Pod
	eventually(t, "a mirror Pod with the static Pod's status", func() bool {
		p, err := mirror()
		if err != nil || p.Status.Phase != corev1.PodRunning {
			return false
		}
		first = p
		return true
	})
	node, err := client.Nodes(c).Get(ctx, "control-plane")
	// Registered not ready (the node lifecycle controller, not running here,
	// lifts that taint) and tainted as control plane.
	if err != nil || node.Labels["node-role.kubernetes.io/control-plane"] != "" || len(node.Spec.Taints) != 2 ||
		node.Spec.Taints[0].Key != corev1.TaintNodeNotReady || node.Spec.Taints[1].Key != corev1.TaintNodeRoleControlPlane ||
		node.Spec.Taints[1].Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("control-plane node registration: %+v %v", node, err)
	}
	if !corev1.IsMirrorPod(first) || first.Spec.NodeName != "control-plane" || len(first.OwnerReferences) != 1 ||
		first.OwnerReferences[0].Kind != "Node" || first.OwnerReferences[0].UID != node.UID {
		t.Fatalf("mirror Pod: %+v", first.ObjectMeta)
	}

	// Deleting the mirror Pod does not stop the static Pod; the mirror returns.
	if err := pods.Delete(ctx, first.Name, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the mirror Pod to be recreated", func() bool {
		p, err := mirror()
		return err == nil && p.UID != first.UID && p.DeletionTimestamp == nil
	})
	if len(rt.kills()) != 0 {
		t.Fatalf("deleting a mirror Pod must not kill the static Pod: %v", rt.kills())
	}

	// Removing the manifest stops the static Pod and deletes its mirror.
	if err := os.Remove(filepath.Join(dir, "kube-apiserver.yaml")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the static Pod to stop and its mirror to go", func() bool {
		_, err := mirror()
		return client.IsNotFound(err) && len(rt.kills()) == 1
	})

	// Putting the same manifest back starts the Pod again (as after
	// `mv kube-apiserver.yaml /tmp` and back).
	writeManifest(t, dir, "kube-apiserver.yaml", apiserverManifest)
	eventually(t, "the restored static Pod and its mirror", func() bool {
		p, err := mirror()
		return err == nil && p.Status.Phase == corev1.PodRunning
	})
}

// The manifests shipped in build/manifests must load, and kube-proxy must
// be the same on the control plane and the workers.
func TestShippedManifestsAreValid(t *testing.T) {
	for dir, want := range map[string][]string{
		"control-plane": {"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "ingress-controller", "kube-proxy"},
		"node":          {"kube-proxy"},
	} {
		pods, errs := LoadStaticPods(filepath.Join("..", "..", "build", "manifests", dir), "n1")
		if len(errs) != 0 {
			t.Fatalf("%s: %v", dir, errs)
		}
		var names []string
		for _, p := range pods {
			names = append(names, strings.TrimSuffix(p.Name, "-n1"))
		}
		if len(names) != len(want) {
			t.Errorf("%s has %v, want %v", dir, names, want)
		}
	}
	a, _ := os.ReadFile(filepath.Join("..", "..", "build", "manifests", "control-plane", "kube-proxy.yaml"))
	b, _ := os.ReadFile(filepath.Join("..", "..", "build", "manifests", "node", "kube-proxy.yaml"))
	if len(a) == 0 || string(a) != string(b) {
		t.Fatal("build/manifests/{control-plane,node}/kube-proxy.yaml must be identical")
	}
}
