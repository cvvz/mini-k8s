package validation

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/pkg/util/validation"
)

// expectErrors validates a Pod spec given as JSON and requires every wanted
// message (as reported by Kubernetes 1.35 for the same spec).
func expectErrors(t *testing.T, specJSON string, want []string) {
	t.Helper()
	var spec corev1.PodSpec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
		t.Fatal(err)
	}
	corev1.SetDefaultsPodSpec(&spec)
	got := ValidatePodSpec("spec", &spec)
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q in:\n%s", w, strings.Join(got, "\n"))
		}
	}
}

func TestEnvValidation(t *testing.T) {
	expectErrors(t, `{"containers": [{"name": "c", "image": "busybox", "env": [
		{"name": "A", "value": "x", "valueFrom": {"configMapKeyRef": {"name": "cm1", "key": "a"}}},
		{"name": "B", "valueFrom": {"fieldRef": {"fieldPath": "metadata.bogus"}}},
		{"name": "C", "valueFrom": {}},
		{"name": "D", "valueFrom": {"resourceFieldRef": {"resource": "limits.bogus"}}},
		{"name": "a=b", "value": "x"},
		{"name": "", "value": "x"},
		{"name": "E", "valueFrom": {"resourceFieldRef": {"resource": "limits.cpu", "divisor": "2"}}},
		{"name": "F", "valueFrom": {"resourceFieldRef": {"resource": "limits.memory", "divisor": "3"}}},
		{"name": "G", "valueFrom": {"configMapKeyRef": {"name": "", "key": "bad key"}}},
		{"name": "H", "valueFrom": {"secretKeyRef": {"name": "s", "key": ""}}},
		{"name": "I", "valueFrom": {"fieldRef": {"fieldPath": "metadata.labels"}}}],
		"envFrom": [{}, {"prefix": "a=", "configMapRef": {"name": "x"}}, {"configMapRef": {"name": ""}}]}]}`,
		[]string{
			"spec.containers[0].env[0].valueFrom: Invalid value: \"\": may not be specified when `value` is not empty",
			`spec.containers[0].env[1].valueFrom.fieldRef.fieldPath: Invalid value: "metadata.bogus": error converting fieldPath: field label not supported: metadata.bogus`,
			"spec.containers[0].env[2].valueFrom: Invalid value: \"\": must specify one of: `fieldRef`, `resourceFieldRef`, `configMapKeyRef` or `secretKeyRef`",
			`spec.containers[0].env[3].valueFrom.resourceFieldRef.resource: Unsupported value: "limits.bogus": supported values: "limits.cpu", "limits.ephemeral-storage", "limits.memory", "requests.cpu", "requests.ephemeral-storage", "requests.memory"`,
			`spec.containers[0].env[4].name: Invalid value: "a=b": a valid environment variable name must consist only of printable ASCII characters other than '='`,
			`spec.containers[0].env[5].name: Required value`,
			`spec.containers[0].env[6].valueFrom.resourceFieldRef.divisor: Invalid value: "limits.cpu": only divisor's values 1m and 1 are supported with the cpu resource`,
			`spec.containers[0].env[7].valueFrom.resourceFieldRef.divisor: Invalid value: "limits.memory": only divisor's values 1, 1k, 1M, 1G, 1T, 1P, 1E, 1Ki, 1Mi, 1Gi, 1Ti, 1Pi, 1Ei are supported with the memory resource`,
			`spec.containers[0].env[8].valueFrom.configMapKeyRef.name: Invalid value: "": ` + validation.DNS1123SubdomainMessage,
			`spec.containers[0].env[8].valueFrom.configMapKeyRef.key: Invalid value: "bad key": ` + validation.ConfigMapKeyMessage,
			`spec.containers[0].env[9].valueFrom.secretKeyRef.key: Required value`,
			`spec.containers[0].env[10].valueFrom.fieldRef.fieldPath: Unsupported value: "metadata.labels": supported values: "metadata.name", "metadata.namespace", "metadata.uid", "spec.nodeName", "spec.serviceAccountName", "status.hostIP", "status.hostIPs", "status.podIP", "status.podIPs"`,
			"spec.containers[0].envFrom: Invalid value: \"\": must specify one of: `configMapRef` or `secretRef`",
			`spec.containers[0].envFrom[1].prefix: Invalid value: "a=": a valid environment variable name must consist only of printable ASCII characters other than '='`,
			`spec.containers[0].envFrom[2].configMapRef.name: Required value`,
		})
	// Valid references, including subscripted labels.
	var spec corev1.PodSpec
	_ = json.Unmarshal([]byte(`{"containers": [{"name": "c", "image": "busybox", "env": [
		{"name": "TIER", "valueFrom": {"fieldRef": {"fieldPath": "metadata.labels['tier']"}}},
		{"name": "IP", "valueFrom": {"fieldRef": {"fieldPath": "status.podIP"}}},
		{"name": "CPU", "valueFrom": {"resourceFieldRef": {"resource": "requests.cpu", "divisor": "1m"}}},
		{"name": "1bad", "valueFrom": {"secretKeyRef": {"name": "s", "key": "k", "optional": true}}}],
		"envFrom": [{"prefix": "S_", "secretRef": {"name": "s"}}]}]}`), &spec)
	corev1.SetDefaultsPodSpec(&spec)
	if errs := ValidatePodSpec("spec", &spec); len(errs) != 0 {
		t.Fatalf("valid env rejected: %v", errs)
	}
	if spec.Containers[0].Env[0].ValueFrom.FieldRef.APIVersion != "v1" {
		t.Fatal("fieldRef.apiVersion defaults to v1")
	}
}

func TestVolumeValidation(t *testing.T) {
	expectErrors(t, `{"containers": [{"name": "c", "image": "busybox"}], "volumes": [
		{"name": "v0", "configMap": {"name": "", "items": [{"key": "", "path": "../x"}], "defaultMode": 512}},
		{"name": "v1", "secret": {}},
		{"name": "v2", "downwardAPI": {"items": [{"path": "/abs", "fieldRef": {"fieldPath": "status.podIP"}}]}},
		{"name": "v3", "configMap": {"name": "a"}, "secret": {"secretName": "b"}},
		{"name": "v4", "projected": {"sources": [
			{"configMap": {"name": "a", "items": [{"key": "k", "path": "dup"}]}},
			{"secret": {"name": "b", "items": [{"key": "k", "path": "dup"}]}}]}},
		{"name": "v5", "downwardAPI": {"items": [{"path": "x"}]}},
		{"name": "v6", "downwardAPI": {"items": [{"path": "y", "resourceFieldRef": {"resource": "limits.cpu"}}]}},
		{"name": "v7", "configMap": {"name": "a", "items": [{"key": "k", "path": "a/../b"}]}},
		{"name": "v8", "secret": {"secretName": "s", "items": [{"key": "k", "path": "x", "mode": 512}]}},
		{"name": "v9"}]}`,
		[]string{
			`spec.volumes[0].configMap.name: Required value`,
			`spec.volumes[0].configMap.defaultMode: Invalid value: 512: must be a number between 0 and 0777 (octal), both inclusive`,
			`spec.volumes[0].configMap.items[0].key: Required value`,
			`spec.volumes[0].configMap.items[0].path: Invalid value: "../x": must not contain '..'`,
			`spec.volumes[1].secret.secretName: Required value`,
			`spec.volumes[2].downwardAPI.path: Invalid value: "/abs": must be a relative path`,
			`spec.volumes[2].downwardAPI.fieldRef.fieldPath: Unsupported value: "status.podIP": supported values: "metadata.annotations", "metadata.labels", "metadata.name", "metadata.namespace", "metadata.uid"`,
			`spec.volumes[3].secret: Forbidden: may not specify more than 1 volume type`,
			`spec.volumes[4].projected: Invalid value: "b": conflicting duplicate paths`,
			`spec.volumes[5].downwardAPI: Required value: one of fieldRef and resourceFieldRef is required`,
			`spec.volumes[6].downwardAPI.resourceFieldRef.containerName: Required value`,
			`spec.volumes[7].configMap.items[0].path: Invalid value: "a/../b": must not contain '..'`,
			`spec.volumes[8].secret.items[0].mode: Invalid value: 512: must be a number between 0 and 0777 (octal), both inclusive`,
			`spec.volumes[9]: Required value: must specify a volume type`,
		})
	spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox",
		VolumeMounts: []corev1.VolumeMount{{Name: "cfg", MountPath: "/etc/cfg"}}}},
		Volumes: []corev1.Volume{{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}, Items: []corev1.KeyToPath{{Key: "k", Path: "sub/dir/file"}}}}}}}
	corev1.SetDefaultsPodSpec(&spec)
	if errs := ValidatePodSpec("spec", &spec); len(errs) != 0 {
		t.Fatalf("valid volume rejected: %v", errs)
	}
	if m := spec.Volumes[0].ConfigMap.DefaultMode; m == nil || *m != 0o644 {
		t.Fatal("defaultMode defaults to 0644")
	}
}

func TestConfigMapAndSecretValidation(t *testing.T) {
	join := func(errs []string) string { return strings.Join(errs, "\n") }
	cm := &corev1.ConfigMap{Data: map[string]string{"a/b": "x", "k": "x"}, BinaryData: map[string][]byte{"k": []byte("x")}}
	want := `data[a/b]: Invalid value: "a/b": ` + validation.ConfigMapKeyMessage + "\n" +
		`data[k]: Invalid value: "k": duplicate of key present in binaryData`
	if got := join(ValidateConfigMap(cm)); got != want {
		t.Errorf("configmap:\n%s\nwant\n%s", got, want)
	}
	big := &corev1.ConfigMap{Data: map[string]string{"k": strings.Repeat("x", corev1.MaxConfigDataSize+1)}}
	if got := join(ValidateConfigMap(big)); got != "data: Too long: must have at most 1048576 bytes" {
		t.Errorf("size: %s", got)
	}

	frozen := &corev1.ConfigMap{Immutable: ptr.To(true), Data: map[string]string{"k": "v"}}
	changed := &corev1.ConfigMap{Immutable: ptr.To(true), Data: map[string]string{"k": "w"}}
	if got := join(ValidateConfigMapUpdate(changed, frozen)); got != "data: Forbidden: field is immutable when `immutable` is set" {
		t.Errorf("immutable data: %s", got)
	}
	thawed := &corev1.ConfigMap{Immutable: ptr.To(false), Data: map[string]string{"k": "v"}}
	if got := join(ValidateConfigMapUpdate(thawed, frozen)); got != "immutable: Forbidden: field is immutable when `immutable` is set" {
		t.Errorf("unsetting immutable: %s", got)
	}
	if errs := ValidateConfigMapUpdate(changed, &corev1.ConfigMap{Data: map[string]string{"k": "v"}}); len(errs) != 0 {
		t.Errorf("a mutable ConfigMap may change and become immutable: %v", errs)
	}

	for typ, want := range map[string]string{
		corev1.SecretTypeTLS:       "data[tls.crt]: Required value\ndata[tls.key]: Required value",
		corev1.SecretTypeBasicAuth: "data[username]: Required value\ndata[password]: Required value",
		corev1.SecretTypeSSHAuth:   "data[ssh-privatekey]: Required value",
		"example.com/custom":       "",
		corev1.SecretTypeOpaque:    "",
	} {
		if got := join(ValidateSecret(&corev1.Secret{Type: typ, Data: map[string][]byte{"other": nil}})); got != want {
			t.Errorf("%s: %q, want %q", typ, got, want)
		}
	}
	docker := &corev1.Secret{Type: corev1.SecretTypeDockerConfigJSON, Data: map[string][]byte{".dockerconfigjson": []byte("not json")}}
	if got := join(ValidateSecret(docker)); !strings.HasPrefix(got, `data[.dockerconfigjson]: Invalid value: "<secret contents redacted>": invalid character`) {
		t.Errorf("dockerconfigjson: %s", got)
	}
	retyped := &corev1.Secret{Type: corev1.SecretTypeTLS}
	if got := join(ValidateSecretUpdate(retyped, &corev1.Secret{Type: corev1.SecretTypeOpaque})); got != `type: Invalid value: "kubernetes.io/tls": field is immutable` {
		t.Errorf("type change: %s", got)
	}
}
