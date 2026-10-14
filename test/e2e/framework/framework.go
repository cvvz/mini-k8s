package framework

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"mini-k8s/internal/mini"
)

// Framework is created once per Describe block. It exposes an API client and
// helpers whose side effects are undone by ginkgo.DeferCleanup.
type Framework struct {
	BaseName string
	Client   *mini.Client
}

// NewDefaultFramework registers BeforeEach/AfterEach hooks for the enclosing
// container, mirroring the Kubernetes e2e framework.
func NewDefaultFramework(baseName string) *Framework {
	f := &Framework{BaseName: baseName}
	ginkgo.BeforeEach(func() {
		f.Client = mini.NewClient(TestContext.APIURL, TestContext.Token)
	})
	// AfterEach runs before DeferCleanup callbacks, so failure diagnostics
	// still see the objects the spec created.
	ginkgo.AfterEach(func(ctx ginkgo.SpecContext) {
		if ginkgo.CurrentSpecReport().Failed() {
			DumpDebugInfo(ctx)
		}
	})
	return f
}

// UniqueName returns a DNS label such as "e2e-deployment-scale-3fa9c1".
func (f *Framework) UniqueName(suffix string) string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		ginkgo.Fail(err.Error())
	}
	name := "e2e-" + f.BaseName
	if suffix != "" {
		name += "-" + suffix
	}
	return name + "-" + hex.EncodeToString(b[:])
}

// SIGDescribe annotates a container with its owning area, e.g. "[sig-apps]".
func SIGDescribe(sig, text string, args ...any) bool {
	return ginkgo.Describe(fmt.Sprintf("[sig-%s] %s", sig, text), args...)
}

// WithDisruptive marks specs that restart or stop cluster components.
func WithDisruptive() ginkgo.Labels { return ginkgo.Label("Disruptive") }

// ExpectNoError fails the spec with context when err is not nil.
func ExpectNoError(err error, explain ...any) {
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred(), explain...)
}

// Logf writes progress to the Ginkgo report.
func Logf(format string, args ...any) {
	ginkgo.GinkgoWriter.Printf(time.Now().Format("15:04:05.000")+" INFO: "+format+"\n", args...)
}

// WaitForClusterReady blocks until the API is healthy and every worker has a
// fresh Ready heartbeat.
func WaitForClusterReady(ctx context.Context, workers int) {
	client := mini.NewClient(TestContext.APIURL, TestContext.Token)
	gomega.Eventually(ctx, func(g gomega.Gomega) {
		g.Expect(client.Health(ctx)).To(gomega.Succeed())
		nodes, err := client.List(ctx, "Node")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		ready := 0
		for _, n := range nodes {
			if NodeReady(n, time.Now()) {
				ready++
			}
		}
		g.Expect(ready).To(gomega.BeNumerically(">=", workers), "Ready workers")
	}).WithTimeout(TestContext.ClusterReadyTimeout).WithPolling(time.Second).Should(gomega.Succeed())
}

// NodeReady reports whether a node is Ready with a heartbeat younger than 15s.
func NodeReady(n mini.Resource, now time.Time) bool {
	return n.Status.Phase == "Ready" && now.Sub(n.Status.Heartbeat) <= 15*time.Second
}
