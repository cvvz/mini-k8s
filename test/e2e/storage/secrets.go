package storage

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("storage", "Secrets", func() {
	f := framework.NewDefaultFramework("secrets")

	It("should provide a Secret as environment variables and as files on a read-only tmpfs", func(ctx SpecContext) {
		Expect(f.KK(ctx, "create", "secret", "generic", "creds", "--from-literal=user=admin", "--from-literal=pass=p@ss")).
			To(Equal("secret/creds created\n"))
		Expect(f.KK(ctx, "get", "secret", "creds", "-o", "yaml")).To(And(ContainSubstring("pass: cEBzcw=="), ContainSubstring("type: Opaque")))
		described := f.KK(ctx, "describe", "secret", "creds")
		Expect(described).To(ContainSubstring("pass:  4 bytes"))
		Expect(described).NotTo(ContainSubstring("p@ss"))

		f.Apply(ctx, newPod("consumer", sleeper(map[string]any{
			"env":          []any{map[string]any{"name": "DB_USER", "valueFrom": keyRef("secretKeyRef", "creds", "user")}},
			"envFrom":      []any{map[string]any{"prefix": "S_", "secretRef": map[string]any{"name": "creds"}}},
			"volumeMounts": []any{mount("creds", "/etc/creds")},
		}), map[string]any{"name": "creds", "secret": map[string]any{"secretName": "creds", "defaultMode": 0o400}}))
		f.WaitForRunningPods(ctx, app("consumer"), 1)
		out, err := execIn(ctx, f, "consumer", "/mini", "env")
		framework.ExpectNoError(err)
		Expect(lines(out)).To(ContainElements("DB_USER=admin", "S_user=admin", "S_pass=p@ss"))
		Expect(execIn(ctx, f, "consumer", "/mini", "cat", "/etc/creds/pass")).To(Equal("p@ss"))
		mounts, err := execIn(ctx, f, "consumer", "/mini", "cat", "/proc/mounts")
		framework.ExpectNoError(err)
		Expect(mounts).To(MatchRegexp(`(?m)^tmpfs /etc/creds tmpfs ro[, ]`), "a secret volume is a read-only tmpfs")

		By("rotating the Secret: the file follows, the environment does not")
		f.Apply(ctx, secret("creds", map[string]any{"pass": "rotated"}))
		Eventually(ctx, func() (string, error) { return execIn(ctx, f, "consumer", "/mini", "cat", "/etc/creds/pass") }).
			WithTimeout(30 * time.Second).Should(Equal("rotated"))
		out, err = execIn(ctx, f, "consumer", "/mini", "env")
		framework.ExpectNoError(err)
		Expect(lines(out)).To(ContainElement("S_pass=p@ss"))
	})

	It("should create TLS Secrets and check the keys of typed Secrets", func(ctx SpecContext) {
		cert, key := tlsPair()
		Expect(f.KK(ctx, "create", "secret", "tls", "web-tls", "--cert", cert, "--key", key)).To(Equal("secret/web-tls created\n"))
		Expect(f.KK(ctx, "get", "secrets")).To(MatchRegexp(`(?m)^web-tls\s+kubernetes.io/tls\s+2\s+`))
		f.Apply(ctx, newPod("tls", sleeper(map[string]any{"volumeMounts": []any{mount("tls", "/etc/tls")}}),
			map[string]any{"name": "tls", "secret": map[string]any{"secretName": "web-tls"}}))
		f.WaitForRunningPods(ctx, app("tls"), 1)
		Expect(execIn(ctx, f, "tls", "/mini", "cat", "/etc/tls/tls.crt")).To(HavePrefix("-----BEGIN CERTIFICATE-----"))

		incomplete := secret("half", map[string]any{"tls.crt": "x"})
		incomplete["type"] = "kubernetes.io/tls"
		Expect(f.ApplyExpectError(ctx, incomplete)).To(ContainSubstring("data[tls.key]: Required value"))
		retyped := secret("web-tls", map[string]any{})
		retyped["type"] = "Opaque"
		Expect(f.ApplyExpectError(ctx, retyped)).To(ContainSubstring(`type: Invalid value: "Opaque": field is immutable`))
	})
})

// tlsPair writes a self-signed certificate and its key as PEM files.
func tlsPair() (string, string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	framework.ExpectNoError(err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "web"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	framework.ExpectNoError(err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	framework.ExpectNoError(err)
	dir := GinkgoT().TempDir()
	cert, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	framework.ExpectNoError(os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	framework.ExpectNoError(os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return cert, keyFile
}
