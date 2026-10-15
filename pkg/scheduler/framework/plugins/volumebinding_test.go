package plugins

import (
	"testing"

	"mini-k8s/pkg/api/resource"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	storagev1 "mini-k8s/pkg/apis/storage/v1"
	"mini-k8s/pkg/scheduler/framework"
	"mini-k8s/pkg/util/ptr"
)

type fakeVolumes struct {
	claims  map[string]*corev1.PersistentVolumeClaim
	volumes []*corev1.PersistentVolume
	classes map[string]*storagev1.StorageClass
	assumed map[string]ClaimAssumption
}

func (f fakeVolumes) Claim(ns, name string) (*corev1.PersistentVolumeClaim, bool) {
	c, ok := f.claims[ns+"/"+name]
	return c, ok
}

func (f fakeVolumes) Volume(name string) (*corev1.PersistentVolume, bool) {
	for _, v := range f.volumes {
		if v.Name == name {
			return v, true
		}
	}
	return nil, false
}

func (f fakeVolumes) Volumes() []*corev1.PersistentVolume { return f.volumes }

func (f fakeVolumes) NodeExists(name string) bool { return name == "worker-1" || name == "worker-2" }

func (f fakeVolumes) Assumed(ns, name string) (ClaimAssumption, bool) {
	a, ok := f.assumed[ns+"/"+name]
	return a, ok
}

func (f fakeVolumes) Class(name string) (*storagev1.StorageClass, bool) {
	c, ok := f.classes[name]
	return c, ok
}

func pvc(name, class, volume string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: name + "-uid"},
		Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []string{corev1.ReadWriteOnce}, StorageClassName: ptr.To(class), VolumeName: volume,
			Resources: corev1.VolumeResources{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
}

func localPV(name, class, node string) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PersistentVolumeSpec{
		Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}, AccessModes: []string{corev1.ReadWriteOnce},
		StorageClassName: class, NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{node}}}}}}}},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable}}
	return pv
}

func withClaims(names ...string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		for _, n := range names {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: n, VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: n}}})
		}
	}
}

func TestVolumeBinding(t *testing.T) {
	wffc := func(name, provisioner string) *storagev1.StorageClass {
		return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: provisioner,
			VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer)}
	}
	lister := fakeVolumes{
		claims: map[string]*corev1.PersistentVolumeClaim{
			"ns/bound":     pvc("bound", "local", "pv-on-2"),
			"ns/dangling":  pvc("dangling", "local", "gone"),
			"ns/immediate": pvc("immediate", "fast", ""),
			"ns/delayed":   pvc("delayed", "local", ""),
			"ns/static":    pvc("static", "manual", ""),
			"ns/static2":   pvc("static2", "manual", ""),
		},
		volumes: []*corev1.PersistentVolume{localPV("pv-on-2", "local", "worker-2"), localPV("manual-on-1", "manual", "worker-1")},
		classes: map[string]*storagev1.StorageClass{
			"local":  wffc("local", "mini-k8s.io/local-path"),
			"manual": wffc("manual", noProvisioner),
			"fast":   {ObjectMeta: metav1.ObjectMeta{Name: "fast"}, Provisioner: "example.com/fast"},
		},
	}
	f := Default()
	vb := VolumeBinding{Lister: lister}
	f.PreFilters = append(f.PreFilters, vb)
	f.Filters = append(f.Filters, vb)
	f.Rand = func(int) int { return 0 }
	w1, w2 := node("worker-1", "2", "4Gi", nil), node("worker-2", "2", "4Gi", nil)
	nodes := []*corev1.Node{w1, w2}
	schedule := func(claims ...string) (string, string) {
		return f.Schedule(pod("p", withClaims(claims...)), framework.NewSnapshot(nodes, nil))
	}
	for _, tc := range []struct {
		claims     []string
		node, want string
	}{
		{[]string{"nope"}, "", `0/2 nodes are available: persistentvolumeclaim "nope" not found.`},
		{[]string{"immediate"}, "", "0/2 nodes are available: pod has unbound immediate PersistentVolumeClaims."},
		{[]string{"bound"}, "worker-2", ""},
		{[]string{"dangling"}, "", "0/2 nodes are available: 2 node(s) unavailable due to one or more pvc(s) bound to non-existent pv(s)."},
		{[]string{"delayed"}, "worker-1", ""},
		{[]string{"bound", "delayed"}, "worker-2", ""},
		{[]string{"static"}, "worker-1", ""},
		{[]string{"static", "static2"}, "", "0/2 nodes are available: 2 node(s) didn't find available persistent volumes to bind."},
		{[]string{"bound", "static"}, "", "0/2 nodes are available: 1 node(s) didn't find available persistent volumes to bind, " +
			"1 node(s) had volume node affinity conflict."},
	} {
		got, msg := schedule(tc.claims...)
		if got != tc.node || msg != tc.want {
			t.Errorf("%v: got %q %q, want %q %q", tc.claims, got, msg, tc.node, tc.want)
		}
	}
	// A claim another Pod is binding, or that has a node selected, keeps
	// the next Pod with it.
	lister.assumed = map[string]ClaimAssumption{"ns/delayed": {Node: "worker-2"}, "ns/static": {Volume: "manual-on-1"}}
	vb.Lister, f.PreFilters[len(f.PreFilters)-1], f.Filters[len(f.Filters)-1] = lister, VolumeBinding{Lister: lister}, VolumeBinding{Lister: lister}
	if got, msg := schedule("delayed"); got != "worker-2" {
		t.Errorf("assumed node: %q %q", got, msg)
	}
	if got, msg := schedule("static"); got != "worker-1" {
		t.Errorf("assumed volume: %q %q", got, msg)
	}
	lister.assumed = nil
	lister.claims["ns/selected"] = pvc("selected", "local", "")
	lister.claims["ns/selected"].Annotations = map[string]string{corev1.AnnSelectedNode: "worker-2"}
	lister.claims["ns/orphaned"] = pvc("orphaned", "local", "")
	lister.claims["ns/orphaned"].Annotations = map[string]string{corev1.AnnSelectedNode: "gone-node"}
	vb.Lister, f.PreFilters[len(f.PreFilters)-1], f.Filters[len(f.Filters)-1] = lister, VolumeBinding{Lister: lister}, VolumeBinding{Lister: lister}
	if got, msg := schedule("selected"); got != "worker-2" {
		t.Errorf("selected node: %q %q", got, msg)
	}
	if got, msg := schedule("orphaned"); got != "worker-1" {
		t.Errorf("a selected node that is gone is ignored: %q %q", got, msg)
	}

	// On worker-1, delayed is provisioned and static gets the local volume.
	bindings, reason := vb.Bindings(pod("p", withClaims("delayed", "static")), w1)
	if reason != "" || len(bindings) != 2 || bindings[0].Volume != nil || bindings[1].Volume == nil || bindings[1].Volume.Name != "manual-on-1" {
		t.Fatalf("bindings: %+v %q", bindings, reason)
	}
}
