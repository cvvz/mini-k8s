package volume

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"mini-k8s/pkg/api/resource"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/ptr"
)

func entries(t *testing.T, dir string) map[string]string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range list {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			target = "<dir>"
		}
		out[e.Name()] = target
	}
	return out
}

func TestAtomicWriterLayoutAndUpdates(t *testing.T) {
	dir := t.TempDir()
	payload := map[string]FileProjection{"a": {Data: []byte("1"), Mode: 0o644}, "sub/dir/file.txt": {Data: []byte("x"), Mode: 0o600}}
	if changed, err := Write(dir, payload); err != nil || !changed {
		t.Fatalf("first write: %v %v", changed, err)
	}
	first := entries(t, dir)
	ts := first["..data"]
	if !strings.HasPrefix(ts, "..20") || first[ts] != "<dir>" || first["a"] != "..data/a" || first["sub"] != "..data/sub" || len(first) != 4 {
		t.Fatalf("layout: %v", first)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "sub", "dir", "file.txt")); err != nil || string(data) != "x" {
		t.Fatalf("nested file: %q %v", data, err)
	}
	info, err := os.Stat(filepath.Join(dir, "sub", "dir", "file.txt"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", info, err)
	}

	if changed, err := Write(dir, payload); err != nil || changed {
		t.Fatalf("unchanged content is not rewritten: %v %v", changed, err)
	}
	if entries(t, dir)["..data"] != ts {
		t.Fatal("..data moved without a change")
	}

	payload = map[string]FileProjection{"a": {Data: []byte("changed"), Mode: 0o644}, "c": {Data: []byte("3"), Mode: 0o644}}
	if changed, err := Write(dir, payload); err != nil || !changed {
		t.Fatalf("update: %v %v", changed, err)
	}
	second := entries(t, dir)
	if second["..data"] == ts || second[ts] != "" || second["sub"] != "" || second["c"] != "..data/c" || len(second) != 4 {
		t.Fatalf("after update (old version and stale links removed): %v", second)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "a")); string(data) != "changed" {
		t.Fatalf("a = %q", data)
	}
	if _, err := Write(dir, map[string]FileProjection{"../escape": {}}); err == nil {
		t.Fatal("a path leaving the volume must be refused")
	}
}

// Paths are cleaned, conflicting ones refused without leaving a partial
// version behind, and versions left by an interrupted write removed.
func TestAtomicWriterPathsAndLeftovers(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "..2020_01_01_00_00_00.42"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := map[string]FileProjection{"./a": {Data: []byte("1"), Mode: 0o644}, "b/": {Data: []byte("2"), Mode: 0o644}}
	if _, err := Write(dir, payload); err != nil {
		t.Fatal(err)
	}
	got := entries(t, dir)
	if got["a"] != "..data/a" || got["b"] != "..data/b" || len(got) != 4 {
		t.Fatalf("cleaned paths and no stale version: %v", got)
	}
	if changed, err := Write(dir, payload); err != nil || changed {
		t.Fatalf("cleaned paths compare equal: %v %v", changed, err)
	}
	for _, bad := range []map[string]FileProjection{
		{"conf": {Data: []byte("x"), Mode: 0o644}, "conf/x": {Data: []byte("y"), Mode: 0o644}},
		{".": {Mode: 0o644}},
		{"a": {Mode: 0o644}, "./a": {Mode: 0o644}},
	} {
		if _, err := Write(dir, bad); err == nil {
			t.Errorf("%v must be refused", bad)
		}
		if after := entries(t, dir); len(after) != 4 {
			t.Fatalf("a refused write changes nothing: %v", after)
		}
	}
}

type objects struct{}

func (objects) ConfigMap(ns, name string) (*corev1.ConfigMap, bool) {
	if ns != "ns" || name != "cm" {
		return nil, false
	}
	return &corev1.ConfigMap{Data: map[string]string{"a": "1", "b": "2"}, BinaryData: map[string][]byte{"bin": {0, 1}}}, true
}

func (objects) Secret(ns, name string) (*corev1.Secret, bool) {
	if ns != "ns" || name != "s" {
		return nil, false
	}
	return &corev1.Secret{Data: map[string][]byte{"user": []byte("admin"), "pass": []byte("p@ss")}}, true
}

func keys(p map[string]FileProjection) string {
	var out []string
	for k, v := range p {
		out = append(out, k+"="+string(v.Data))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestPayloads(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "good", Namespace: "ns", UID: "u1",
		Labels: map[string]string{"tier": "x", "app": "good"}, Annotations: map[string]string{"note": `say "hi"`}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}}}}}}
	s := Source{Pod: pod, Objects: objects{}, Allocatable: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
	ref := func(name string) corev1.LocalObjectReference { return corev1.LocalObjectReference{Name: name} }
	for name, tc := range map[string]struct {
		v    corev1.VolumeSource
		want string
		err  string
	}{
		"all keys":      {v: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: ref("cm")}}, want: "a=1,b=2,bin=\x00\x01"},
		"items":         {v: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: ref("cm"), Items: []corev1.KeyToPath{{Key: "b", Path: "sub/b"}}}}, want: "sub/b=2"},
		"missing":       {v: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: ref("nope")}}, err: `configmap "nope" not found`},
		"optional":      {v: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: ref("nope"), Optional: ptr.To(true)}}, want: ""},
		"missing key":   {v: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: ref("cm"), Items: []corev1.KeyToPath{{Key: "zzz", Path: "z"}}}}, err: "configmap references non-existent config key: zzz"},
		"optional key":  {v: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: ref("cm"), Optional: ptr.To(true), Items: []corev1.KeyToPath{{Key: "zzz", Path: "z"}, {Key: "a", Path: "a"}}}}, want: "a=1"},
		"secret":        {v: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}}, want: "pass=p@ss,user=admin"},
		"secret absent": {v: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "x"}}, err: `secret "x" not found`},
		"downward": {v: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{Items: []corev1.DownwardAPIVolumeFile{
			{Path: "labels", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels"}},
			{Path: "annotations", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.annotations"}},
			{Path: "tier", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels['tier']"}},
			{Path: "cpu", ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: "c", Resource: "limits.cpu", Divisor: ptr.To(resource.MustParse("1m"))}},
			{Path: "mem", ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: "c", Resource: "limits.memory", Divisor: ptr.To(resource.MustParse("1Mi"))}}}}},
			want: `annotations=note="say \"hi\"",cpu=250,labels=app="good"` + "\n" + `tier="x",mem=1024,tier=x`},
		"projected": {v: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
			{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: ref("cm"), Items: []corev1.KeyToPath{{Key: "a", Path: "cm-a"}}}},
			{Secret: &corev1.SecretProjection{LocalObjectReference: ref("s")}},
			{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "ns", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}}}}}},
			want: "cm-a=1,ns=ns,pass=p@ss,user=admin"},
	} {
		payload, ok, err := s.Payload(tc.v)
		if !ok {
			t.Fatalf("%s: not a projected type", name)
		}
		if tc.err != "" {
			if err == nil || err.Error() != tc.err {
				t.Errorf("%s: error %v, want %q", name, err, tc.err)
			}
			continue
		}
		if err != nil || keys(payload) != tc.want {
			t.Errorf("%s: %q %v, want %q", name, keys(payload), err, tc.want)
		}
	}
	modes, _, _ := s.Payload(corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s", DefaultMode: ptr.To[int32](0o400),
		Items: []corev1.KeyToPath{{Key: "user", Path: "u"}, {Key: "pass", Path: "p", Mode: ptr.To[int32](0o600)}}}})
	if modes["u"].Mode != 0o400 || modes["p"].Mode != 0o600 {
		t.Errorf("modes: %+v", modes)
	}
	if _, ok, _ := s.Payload(corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}); ok {
		t.Error("emptyDir has no payload")
	}
}
