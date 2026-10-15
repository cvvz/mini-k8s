package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/integration/framework"
)

func TestCreateGetDescribeConfigMapsAndSecrets(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	for name, content := range map[string]string{"one": "one\n", "two.txt": "two\n", "blob": "\x00\x01\xff"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(argv ...string) string {
		t.Helper()
		out, err := kk(t, c, "", argv...)
		if err != nil {
			t.Fatalf("kk %v: %v", argv, err)
		}
		return out
	}
	if out := run("create", "configmap", "app", "--from-literal=a=1", "--from-literal=b=", "--from-file=renamed="+filepath.Join(dir, "one"),
		"--from-file="+dir); out != "configmap/app created\n" {
		t.Fatalf("create: %q", out)
	}
	cm, err := client.ConfigMaps(c, "default").Get(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data["a"] != "1" || cm.Data["b"] != "" || cm.Data["renamed"] != "one\n" || cm.Data["two.txt"] != "two\n" ||
		string(cm.BinaryData["blob"]) != "\x00\x01\xff" || len(cm.Data)+len(cm.BinaryData) != 6 {
		t.Fatalf("data %v binary %v", cm.Data, cm.BinaryData)
	}
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"create", "configmap", "bad", "--from-literal=a=1", "--from-literal=a=2"},
			`cannot add key "a", another key by that name already exists in Data for ConfigMap "bad"`},
		{[]string{"create", "configmap", "bad", "--from-literal=has space=1"}, `"has space" is not a valid key name for a ConfigMap: a valid config key`},
		{[]string{"create", "configmap", "app", "--from-literal=a=1"}, `failed to create configmap: configmaps "app" already exists`},
		{[]string{"create", "secret", "opaque", "x"}, "usage: kk create secret generic NAME"},
	} {
		if _, err := kk(t, c, "", tc.argv...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.argv, err, tc.want)
		}
	}

	run("create", "secret", "generic", "creds", "--from-literal=user=admin", "--from-literal=pass=p@ss")
	run("create", "secret", "generic", "custom", "--type", "example.com/x", "--from-literal=a=b")
	certFile, keyFile := selfSignedPair(t)
	if out := run("create", "secret", "tls", "web-tls", "--cert", certFile, "--key", keyFile); out != "secret/web-tls created\n" {
		t.Fatalf("tls: %q", out)
	}
	if _, err := kk(t, c, "", "create", "secret", "tls", "bad-tls", "--cert", certFile, "--key", certFile); err == nil ||
		!strings.HasPrefix(err.Error(), "tls: ") {
		t.Errorf("a mismatched pair must be refused: %v", err)
	}
	s, err := client.Secrets(c, "default").Get(ctx, "web-tls")
	if err != nil || s.Type != corev1.SecretTypeTLS || len(s.Data[corev1.TLSCertKey]) == 0 {
		t.Fatalf("tls secret: %v %+v", err, s)
	}

	get := run("get", "secrets")
	for _, line := range []string{"NAME", "creds     Opaque              2", "custom    example.com/x       1", "web-tls   kubernetes.io/tls   2"} {
		if !strings.Contains(get, line) {
			t.Errorf("kk get secrets lacks %q:\n%s", line, get)
		}
	}
	if get := run("get", "cm"); !strings.Contains(get, "app    6      ") {
		t.Errorf("kk get cm:\n%s", get)
	}
	desc := run("describe", "secret", "creds")
	if !strings.Contains(desc, "Type:  Opaque\n\nData\n====\npass:  4 bytes\nuser:  5 bytes\n") || strings.Contains(desc, "admin") {
		t.Errorf("describe secret must show sizes only:\n%s", desc)
	}
	desc = run("describe", "configmap", "app")
	if !strings.Contains(desc, "Data\n====\na:\n----\n1\n\nb:\n----\n\n\n") || !strings.Contains(desc, "BinaryData\n====\nblob: 3 bytes\n") {
		t.Errorf("describe configmap:\n%s", desc)
	}
}

// selfSignedPair writes a certificate and its key as PEM files.
func selfSignedPair(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "web"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
