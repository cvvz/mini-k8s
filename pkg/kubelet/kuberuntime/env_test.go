package kuberuntime

import (
	"encoding/json"
	"testing"

	"mini-k8s/pkg/api/resource"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

// fakeObjects serves ConfigMaps and Secrets by namespace/name.
type fakeObjects struct {
	configMaps map[string]*corev1.ConfigMap
	secrets    map[string]*corev1.Secret
}

func (f fakeObjects) ConfigMap(ns, name string) (*corev1.ConfigMap, bool) {
	cm, ok := f.configMaps[ns+"/"+name]
	return cm, ok
}

func (f fakeObjects) Secret(ns, name string) (*corev1.Secret, bool) {
	s, ok := f.secrets[ns+"/"+name]
	return s, ok
}

func testObjects() fakeObjects {
	return fakeObjects{
		configMaps: map[string]*corev1.ConfigMap{
			"ns/cm1":     {Data: map[string]string{"a": "1", "b": "2"}},
			"ns/envkeys": {Data: map[string]string{"1bad": "x", "a-b": "y", "GOOD": "z"}},
		},
		secrets: map[string]*corev1.Secret{"ns/s1": {Data: map[string][]byte{"user": []byte("admin"), "pass": []byte("p@ss")}}},
	}
}

// The environment of the AKS probe Pod ("good"), minus what busybox adds.
func TestContainerEnvMatchesKubernetes(t *testing.T) {
	var pod corev1.Pod
	if err := json.Unmarshal([]byte(`{"metadata": {"name": "good", "namespace": "ns", "uid": "u1", "labels": {"tier": "x"}},
		"spec": {"nodeName": "node-1", "containers": [{"name": "c", "image": "busybox",
		"command": ["sh", "-c", "echo $(A) $(MISSING)"], "args": ["$(POD_NAME)", "$$(A)"],
		"resources": {"requests": {"cpu": "100m", "memory": "32Mi"}, "limits": {"cpu": "250m", "memory": "64Mi"}},
		"env": [
			{"name": "A", "valueFrom": {"configMapKeyRef": {"name": "cm1", "key": "a"}}},
			{"name": "EXP", "value": "a is $(A), b is $(B), esc $$(A)"},
			{"name": "OPT", "valueFrom": {"configMapKeyRef": {"name": "nope", "key": "a", "optional": true}}},
			{"name": "OPTKEY", "valueFrom": {"secretKeyRef": {"name": "s1", "key": "nope", "optional": true}}},
			{"name": "POD_NAME", "valueFrom": {"fieldRef": {"fieldPath": "metadata.name"}}},
			{"name": "POD_IP", "valueFrom": {"fieldRef": {"fieldPath": "status.podIP"}}},
			{"name": "HOST_IP", "valueFrom": {"fieldRef": {"fieldPath": "status.hostIP"}}},
			{"name": "NODE", "valueFrom": {"fieldRef": {"fieldPath": "spec.nodeName"}}},
			{"name": "TIER", "valueFrom": {"fieldRef": {"fieldPath": "metadata.labels['tier']"}}},
			{"name": "CPU_LIMIT", "valueFrom": {"resourceFieldRef": {"resource": "limits.cpu"}}},
			{"name": "CPU_REQ_M", "valueFrom": {"resourceFieldRef": {"resource": "requests.cpu", "divisor": "1m"}}},
			{"name": "MEM_LIMIT_MI", "valueFrom": {"resourceFieldRef": {"resource": "limits.memory", "divisor": "1Mi"}}},
			{"name": "EPH", "valueFrom": {"resourceFieldRef": {"resource": "limits.ephemeral-storage", "divisor": "1Gi"}}},
			{"name": "GOOD", "value": "overrides envFrom"},
			{"name": "SVC_COPY", "value": "$(KUBERNETES_SERVICE_HOST)"}],
		"envFrom": [{"configMapRef": {"name": "envkeys"}}, {"prefix": "S_", "secretRef": {"name": "s1"}}]}]}}`), &pod); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Objects: testObjects(), NodeIP: "172.18.0.3",
		Allocatable: corev1.ResourceList{"ephemeral-storage": resource.MustParse("10Gi")}}
	services := []corev1.EnvVar{{Name: "KUBERNETES_SERVICE_HOST", Value: "10.96.0.1"}, {Name: "GOOD", Value: "service"}}
	env, err := m.containerEnv(&pod, pod.Spec.Containers[0], services, "10.244.1.5")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range env {
		if _, dup := got[e.Name]; dup {
			t.Errorf("%s is set twice", e.Name)
		}
		got[e.Name] = e.Value
	}
	want := map[string]string{
		"KUBERNETES_SERVICE_HOST": "10.96.0.1", "1bad": "x", "a-b": "y", "GOOD": "overrides envFrom", "S_pass": "p@ss", "S_user": "admin",
		"A": "1", "EXP": "a is 1, b is $(B), esc $(A)", "POD_NAME": "good", "POD_IP": "10.244.1.5", "HOST_IP": "172.18.0.3",
		"NODE": "node-1", "TIER": "x", "CPU_LIMIT": "1", "CPU_REQ_M": "100", "MEM_LIMIT_MI": "64", "EPH": "10", "SVC_COPY": "10.96.0.1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for _, absent := range []string{"OPT", "OPTKEY"} {
		if _, ok := got[absent]; ok {
			t.Errorf("optional %s must be skipped", absent)
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected variables: %v", got)
	}
	cfg := containerConfig(&pod, pod.Spec.Containers[0], 0, mountedVolumes{}, containerExtras{env: env})
	if cfg.Command[2] != "echo 1 $(MISSING)" || cfg.Args[0] != "good" || cfg.Args[1] != "$(A)" {
		t.Fatalf("command %q args %q", cfg.Command, cfg.Args)
	}
}

func TestContainerEnvErrors(t *testing.T) {
	m := &Manager{Objects: testObjects()}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	for want, c := range map[string]corev1.Container{
		`configmap "nope" not found`: {Env: []corev1.EnvVar{{Name: "A", ValueFrom: &corev1.EnvVarSource{
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "nope"}, Key: "a"}}}}},
		"couldn't find key zzz in ConfigMap ns/cm1": {Env: []corev1.EnvVar{{Name: "A", ValueFrom: &corev1.EnvVarSource{
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "cm1"}, Key: "zzz"}}}}},
		`secret "nosecret" not found`: {EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "nosecret"}}}}},
		"couldn't find key zzz in Secret ns/s1": {Env: []corev1.EnvVar{{Name: "A", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s1"}, Key: "zzz"}}}}},
	} {
		if _, err := m.containerEnv(pod, c, nil, ""); err == nil || err.Error() != want {
			t.Errorf("got %v, want %q", err, want)
		}
	}
	optional := true
	c := corev1.Container{EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "nope"}, Optional: &optional}}}}
	if env, err := m.containerEnv(pod, c, nil, ""); err != nil || len(env) != 0 {
		t.Fatalf("an optional missing envFrom source adds nothing: %v %v", env, err)
	}
}
