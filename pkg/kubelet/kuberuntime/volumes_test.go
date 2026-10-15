package kuberuntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"mini-k8s/pkg/api/resource"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

type fakeMounter struct {
	mu      sync.Mutex
	mounted map[string]int64
	mounts  int
}

func (f *fakeMounter) MountTmpfs(dir string, size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounted[dir], f.mounts = size, f.mounts+1
	return nil
}

func (f *fakeMounter) Unmount(dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.mounted, dir)
	return nil
}

func ptrTo(s string) *string { return &s }

func TestSetupVolumes(t *testing.T) {
	_, m, _ := newTestManager(t)
	host := t.TempDir()
	size := resource.MustParse("16Mi")
	pod := testPod()
	pod.Spec.Volumes = []corev1.Volume{
		{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: "Memory", SizeLimit: &size}}},
		{Name: "data", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: filepath.Join(host, "data"), Type: ptrTo("DirectoryOrCreate")}}},
		{Name: "socket", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: filepath.Join(host, "run", "x.sock"), Type: ptrTo("FileOrCreate")}}},
	}
	vols, err := m.setupVolumes(pod)
	if err != nil {
		t.Fatal(err)
	}
	paths := vols.paths
	scratch := filepath.Join(m.PodsDir, "uid-1", "volumes", "kubernetes.io~empty-dir", "scratch")
	if info, err := os.Stat(scratch); err != nil || paths["scratch"] != scratch || info.Mode().Perm() != 0o777 {
		t.Fatalf("emptyDir: %v %v", paths, err)
	}
	if info, err := os.Stat(paths["data"]); err != nil || !info.IsDir() {
		t.Fatalf("DirectoryOrCreate: %v", err)
	}
	if info, err := os.Stat(paths["socket"]); err != nil || info.IsDir() {
		t.Fatalf("FileOrCreate: %v", err)
	}
	fm := m.Mounter.(*fakeMounter)
	if fm.mounted[paths["cache"]] != 16<<20 {
		t.Fatalf("memory emptyDir must be a tmpfs of its size limit: %v", fm.mounted)
	}
	if _, err := m.setupVolumes(pod); err != nil || fm.mounts != 1 {
		t.Fatalf("setup is idempotent: mounts=%d %v", fm.mounts, err)
	}

	for typ, want := range map[string]string{"Directory": "is not a directory", "File": "is not a file"} {
		bad := testPod()
		bad.Spec.Volumes = []corev1.Volume{{Name: "x", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
			Path: filepath.Join(host, "missing"), Type: ptrTo(typ)}}}}
		if _, err := m.setupVolumes(bad); err == nil || !strings.Contains(err.Error(), `MountVolume.SetUp failed for volume "x" : hostPath type check failed:`) ||
			!strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", typ, err)
		}
	}
}

// configMap, secret, downwardAPI and projected volumes: written under the
// Pod's directory, in memory except for configMaps, mounted read-only,
// refreshed on every setup; a missing object fails the setup and is
// reported as FailedMount events, paced by a backoff.
func TestProjectedVolumes(t *testing.T) {
	_, m, _ := newTestManager(t)
	objects := testObjects()
	m.Objects = objects
	rec := &fakeRecorder{}
	m.Recorder = rec
	pod := testPod()
	pod.Namespace = "ns"
	pod.Labels = map[string]string{"tier": "x"}
	pod.Spec.Volumes = []corev1.Volume{
		{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cm1"}}}},
		{Name: "sec", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s1"}}},
		{Name: "down", VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{Items: []corev1.DownwardAPIVolumeFile{
			{Path: "labels", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels"}}}}}},
	}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "cfg", MountPath: "/etc/cfg"}, {Name: "sec", MountPath: "/etc/sec"}}
	vols, err := m.setupVolumes(pod)
	if err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(m.PodsDir, "uid-1", "volumes", "kubernetes.io~configmap", "cfg")
	if vols.paths["cfg"] != cfgDir || !vols.readOnly["cfg"] || !vols.readOnly["sec"] || !vols.readOnly["down"] {
		t.Fatalf("volumes: %+v", vols)
	}
	if data, err := os.ReadFile(filepath.Join(cfgDir, "a")); err != nil || string(data) != "1" {
		t.Fatalf("configMap file: %q %v", data, err)
	}
	fm := m.Mounter.(*fakeMounter)
	secDir := filepath.Join(m.PodsDir, "uid-1", "volumes", "kubernetes.io~secret", "sec")
	downDir := filepath.Join(m.PodsDir, "uid-1", "volumes", "kubernetes.io~downward-api", "down")
	if _, ok := fm.mounted[secDir]; !ok || len(fm.mounted) != 2 {
		t.Fatalf("secret and downward API volumes are tmpfs, configMaps are not: %v", fm.mounted)
	}
	if data, _ := os.ReadFile(filepath.Join(downDir, "labels")); string(data) != `tier="x"` {
		t.Fatalf("labels file: %q", data)
	}
	cfg := containerConfig(pod, pod.Spec.Containers[0], 0, vols, containerExtras{})
	if len(cfg.Mounts) != 2 || !cfg.Mounts[0].Readonly || !cfg.Mounts[1].Readonly {
		t.Fatalf("projected volumes are mounted read-only: %+v", cfg.Mounts)
	}

	objects.configMaps["ns/cm1"].Data["a"] = "changed"
	pod.Labels["tier"] = "y"
	if _, err := m.setupVolumes(pod); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(cfgDir, "a")); string(data) != "changed" {
		t.Fatalf("configMap update: %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(downDir, "labels")); string(data) != `tier="y"` {
		t.Fatalf("label update: %q", data)
	}

	missing := testPod()
	missing.UID = "uid-2"
	missing.Spec.Volumes = []corev1.Volume{{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "nope"}}}}}
	for range 3 {
		_, err := m.setupVolumes(missing)
		m.reportMountFailure(missing, err)
		if err == nil || err.Error() != `MountVolume.SetUp failed for volume "cfg" : configmap "nope" not found` {
			t.Fatalf("missing configMap: %v", err)
		}
	}
	if len(rec.events) != 1 || rec.events[0] != `Warning FailedMount MountVolume.SetUp failed for volume "cfg" : configmap "nope" not found` {
		t.Fatalf("one FailedMount event per backoff period: %q", rec.events)
	}
}

type fakeRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *fakeRecorder) Eventf(_ metav1.Object, eventType, reason, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, eventType+" "+reason+" "+fmt.Sprintf(format, args...))
}

func TestVolumeMountsAndNamespacesInCRIConfig(t *testing.T) {
	_, m, _ := newTestManager(t)
	pod := testPod()
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data", ReadOnly: true}}
	cfg := containerConfig(pod, pod.Spec.Containers[0], 0, mountedVolumes{paths: map[string]string{"data": "/var/lib/x"}}, containerExtras{})
	if len(cfg.Mounts) != 1 || cfg.Mounts[0].HostPath != "/var/lib/x" || cfg.Mounts[0].ContainerPath != "/data" || !cfg.Mounts[0].Readonly {
		t.Fatalf("mounts: %+v", cfg.Mounts)
	}
	ns := cfg.Linux.SecurityContext.NamespaceOptions
	if ns.Network != runtimeapi.NamespaceMode_POD || ns.Pid != runtimeapi.NamespaceMode_CONTAINER || ns.Ipc != runtimeapi.NamespaceMode_POD {
		t.Fatalf("namespaces: %+v", ns)
	}
	m.DNS = func(p *corev1.Pod) *runtimeapi.DNSConfig {
		if p.Spec.HostNetwork {
			return nil // the node's resolver, as the DNS configurer decides
		}
		return &runtimeapi.DNSConfig{Servers: []string{"172.18.0.5"}}
	}
	if sb := m.sandboxConfig(pod, 0); sb.Hostname != "web" || sb.DnsConfig == nil {
		t.Fatalf("pod network sandbox: %+v", sb)
	}
	pod.Spec.HostNetwork = true
	sb := m.sandboxConfig(pod, 0)
	if sb.Linux.SecurityContext.NamespaceOptions.Network != runtimeapi.NamespaceMode_NODE || sb.Hostname != "" || sb.DnsConfig != nil {
		t.Fatalf("host network sandbox: %+v", sb)
	}
	if c := containerConfig(pod, pod.Spec.Containers[0], 0, mountedVolumes{}, containerExtras{}); c.Linux.SecurityContext.NamespaceOptions.Network != runtimeapi.NamespaceMode_NODE {
		t.Fatal("host network container")
	}
}

func TestVolumeErrorsBlockOnlyNewContainersAndPodDirsAreRemoved(t *testing.T) {
	f, m, _ := newTestManager(t)
	pod := testPod()
	pod.Spec.Volumes = []corev1.Volume{{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: "Memory"}}}}
	syncOnce(m, pod)
	if pod.Status.ContainerStatuses[0].State.Running == nil {
		t.Fatalf("setup: %+v", pod.Status)
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "gone", VolumeSource: corev1.VolumeSource{
		HostPath: &corev1.HostPathVolumeSource{Path: "/nonexistent-mini-k8s", Type: ptrTo("Directory")}}})
	syncOnce(m, pod)
	if pod.Status.ContainerStatuses[0].State.Running == nil {
		t.Fatal("a volume error must not affect a running container")
	}
	f.exit(1)
	syncOnce(m, pod)
	syncOnce(m, pod)
	if w := pod.Status.ContainerStatuses[0].State.Waiting; w == nil {
		t.Fatalf("restart must be blocked: %+v", pod.Status.ContainerStatuses[0])
	}

	other := testPod()
	other.UID = "uid-2"
	if err := os.MkdirAll(m.emptyDirPath("uid-2", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Cleanup(context.Background(), func() map[string]bool { return map[string]bool{"uid-1": true} }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.podDir("uid-2")); !os.IsNotExist(err) {
		t.Fatal("orphaned Pod directory kept")
	}

	// A Pod that starts while housekeeping runs (after the listing, before
	// the keep set is known) must not lose its volumes.
	late := func() map[string]bool {
		if err := os.MkdirAll(m.emptyDirPath("uid-late", "data"), 0o755); err != nil {
			t.Fatal(err)
		}
		return map[string]bool{"uid-1": true}
	}
	if err := m.Cleanup(context.Background(), late); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.emptyDirPath("uid-late", "data")); err != nil {
		t.Fatalf("volume of a Pod that arrived during housekeeping was removed: %v", err)
	}
	if err := m.KillPod(context.Background(), pod, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.podDir("uid-1")); !os.IsNotExist(err) || len(m.Mounter.(*fakeMounter).mounted) != 0 {
		t.Fatalf("killed Pod left its volumes: %v %v", err, m.Mounter.(*fakeMounter).mounted)
	}
}

func TestSecurityContextInCRIConfig(t *testing.T) {
	_, m, _ := newTestManager(t)
	pod := testPod()
	if cfg := containerConfig(pod, pod.Spec.Containers[0], 0, mountedVolumes{}, containerExtras{}); cfg.Linux.SecurityContext.Privileged || m.sandboxConfig(pod, 0).Linux.SecurityContext.Privileged {
		t.Fatal("containers are unprivileged by default")
	}
	privileged := true
	pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &privileged,
		Capabilities: &corev1.Capabilities{Add: []string{"NET_ADMIN"}, Drop: []string{"MKNOD"}}}
	sc := containerConfig(pod, pod.Spec.Containers[0], 0, mountedVolumes{}, containerExtras{}).Linux.SecurityContext
	if !sc.Privileged || sc.Capabilities.AddCapabilities[0] != "NET_ADMIN" || sc.Capabilities.DropCapabilities[0] != "MKNOD" {
		t.Fatalf("security context: %+v", sc)
	}
	if !m.sandboxConfig(pod, 0).Linux.SecurityContext.Privileged {
		t.Fatal("a privileged container needs a privileged sandbox")
	}
}
