package statefulset

import (
	"testing"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/ptr"
)

func testSet() *appsv1.StatefulSet {
	labels := map[string]string{"app": "db"}
	s := &appsv1.StatefulSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", UID: "set-uid"},
		Spec: appsv1.StatefulSetSpec{ServiceName: "db-svc", Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "db", "tier": "data"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
					Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}}},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data", Labels: map[string]string{"extra": "x"}},
				Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []string{corev1.ReadWriteOnce}}}}}}
	appsv1.SetDefaultsStatefulSet(s)
	return s
}

func TestNewPodIdentity(t *testing.T) {
	s := testSet()
	p := newPod(s, &s.Spec.Template, "db-abc", 2)
	if p.Name != "db-2" || p.Namespace != "ns" || p.Spec.Hostname != "db-2" || p.Spec.Subdomain != "db-svc" {
		t.Fatalf("identity: %s %s %s.%s", p.Namespace, p.Name, p.Spec.Hostname, p.Spec.Subdomain)
	}
	for k, v := range map[string]string{"app": "db", "tier": "data", appsv1.StatefulSetPodNameLabel: "db-2",
		appsv1.PodIndexLabel: "2", appsv1.ControllerRevisionHashLabelKey: "db-abc"} {
		if p.Labels[k] != v {
			t.Errorf("label %s=%q, want %q", k, p.Labels[k], v)
		}
	}
	if ref := metav1.ControllerOf(&p.ObjectMeta); ref == nil || ref.UID != "set-uid" {
		t.Fatalf("owner: %+v", p.OwnerReferences)
	}
	// The claim replaces the template's volume of the same name.
	if len(p.Spec.Volumes) != 2 || p.Spec.Volumes[0].PersistentVolumeClaim == nil ||
		p.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "data-db-2" || p.Spec.Volumes[1].EmptyDir == nil {
		t.Fatalf("volumes: %+v", p.Spec.Volumes)
	}
	if s.Spec.Template.Spec.Volumes[0].EmptyDir == nil || s.Spec.Template.Labels[appsv1.PodIndexLabel] != "" {
		t.Fatal("the template must not change")
	}
	if !identityMatches(s, p, 2) || identityMatches(s, p, 3) {
		t.Fatal("identityMatches")
	}
	if getOrdinal(s, p) != 2 || getOrdinal(s, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "dbx-1"}}) != -1 ||
		getOrdinal(s, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-1-a"}}) != -1 {
		t.Fatal("getOrdinal")
	}
	// Without a governing Service the Pod has no subdomain.
	s.Spec.ServiceName = ""
	if p := newPod(s, &s.Spec.Template, "r", 0); p.Spec.Subdomain != "" || p.Spec.Hostname != "db-0" {
		t.Fatalf("no service: %s.%s", p.Spec.Hostname, p.Spec.Subdomain)
	}
}

func TestNewClaim(t *testing.T) {
	s := testSet()
	c := newClaim(s, &s.Spec.VolumeClaimTemplates[0], 1)
	if c.Name != "data-db-1" || c.Namespace != "ns" || c.Labels["extra"] != "x" || c.Labels["app"] != "db" || c.Labels["tier"] != "" {
		t.Fatalf("claim: %+v", c.ObjectMeta)
	}
	if *c.Spec.VolumeMode != corev1.PersistentVolumeFilesystem || c.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("claim spec: %+v", c.Spec)
	}
}

func TestClaimOwnerRefs(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-3", UID: "pod-uid"}}
	other := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "other"}
	kinds := func(refs []metav1.OwnerReference) string {
		out := ""
		for _, r := range refs {
			out += r.Kind
			if ptr.Deref(r.Controller, false) {
				out += "*"
			}
			out += ","
		}
		return out
	}
	for _, tc := range []struct {
		whenDeleted, whenScaled string
		scaledDown              bool
		want                    string
	}{
		{"Retain", "Retain", false, "ConfigMap,"},
		{"Retain", "Retain", true, "ConfigMap,"},
		{"Delete", "Retain", false, "ConfigMap,StatefulSet*,"},
		{"Delete", "Retain", true, "ConfigMap,StatefulSet*,"},
		{"Retain", "Delete", false, "ConfigMap,"},
		{"Retain", "Delete", true, "ConfigMap,Pod,"},
		{"Delete", "Delete", false, "ConfigMap,StatefulSet*,"},
		{"Delete", "Delete", true, "ConfigMap,Pod,"},
	} {
		s := testSet()
		s.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted = tc.whenDeleted
		s.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled = tc.whenScaled
		// Start from the opposite references to see them corrected.
		start := []metav1.OwnerReference{other, metav1.NewControllerRef(s), {Kind: "Pod", Name: "db-3", UID: "pod-uid"}}
		got := claimOwnerRefs(s, pod, tc.scaledDown, start)
		if kinds(got) != tc.want {
			t.Errorf("%s/%s scaled down %v: %s, want %s", tc.whenDeleted, tc.whenScaled, tc.scaledDown, kinds(got), tc.want)
		}
		if !sameOwners(got, claimOwnerRefs(s, pod, tc.scaledDown, got)) {
			t.Errorf("%s/%s: not stable", tc.whenDeleted, tc.whenScaled)
		}
	}
	if sameOwners([]metav1.OwnerReference{{UID: "a"}}, []metav1.OwnerReference{{UID: "b"}}) {
		t.Fatal("sameOwners")
	}
}
