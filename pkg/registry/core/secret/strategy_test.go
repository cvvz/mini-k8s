package secret_test

import (
	"context"
	"strings"
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/test/integration/framework"
)

func TestStringDataTypeAndImmutability(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := context.Background()
	secrets := client.Secrets(c, "default")
	s, err := secrets.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sd"},
		Data: map[string][]byte{"a": []byte("old"), "b": []byte("b")}, StringData: map[string]string{"a": "new", "c": "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Type != corev1.SecretTypeOpaque || s.StringData != nil || string(s.Data["a"]) != "new" || string(s.Data["c"]) != "c" || len(s.Data) != 3 {
		t.Fatalf("stringData merges into data and is not kept: %+v", s)
	}
	list, err := secrets.List(ctx, client.ListOptions{FieldSelector: "type=Opaque"})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("field selector on type: %v %v", err, list)
	}

	s.Type = corev1.SecretTypeTLS
	s.Data = map[string][]byte{corev1.TLSCertKey: []byte("c"), corev1.TLSPrivateKeyKey: []byte("k")}
	if _, err := secrets.Update(ctx, s); !client.IsInvalid(err) || !strings.Contains(err.Error(), `type: Invalid value: "kubernetes.io/tls": field is immutable`) {
		t.Fatalf("type change: %v", err)
	}

	cms := client.ConfigMaps(c, "default")
	cm, err := cms.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "frozen"}, Immutable: ptr.To(true),
		Data: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data["k"] = "w"
	if _, err := cms.Update(ctx, cm); !client.IsInvalid(err) || !strings.Contains(err.Error(), "data: Forbidden: field is immutable when `immutable` is set") {
		t.Fatalf("immutable data: %v", err)
	}
	cm.Data["k"] = "v"
	cm.Labels = map[string]string{"x": "y"}
	if _, err := cms.Update(ctx, cm); err != nil {
		t.Fatalf("metadata of an immutable ConfigMap may change: %v", err)
	}
}
