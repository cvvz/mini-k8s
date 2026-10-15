package framework

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
)

// Framework is created once per Describe block. Every spec gets a fresh
// namespace that is deleted (with all its contents) when the spec ends.
type Framework struct {
	BaseName  string
	Client    *client.Client
	Namespace string
}

// NewDefaultFramework registers BeforeEach/AfterEach hooks for the enclosing
// container, mirroring the Kubernetes e2e framework.
func NewDefaultFramework(baseName string) *Framework {
	f := &Framework{BaseName: baseName}
	ginkgo.BeforeEach(func(ctx ginkgo.SpecContext) {
		f.Client = client.New(TestContext.APIURL, TestContext.Token)
		f.Namespace = f.CreateNamespace(ctx, "")
	})
	// AfterEach runs before DeferCleanup callbacks, so failure diagnostics
	// still see the objects the spec created.
	ginkgo.AfterEach(func(ctx ginkgo.SpecContext) {
		if ginkgo.CurrentSpecReport().Failed() {
			DumpDebugInfo(ctx, f.Namespace)
		}
	})
	return f
}

// CreateNamespace creates a uniquely named namespace and registers its
// deletion. An empty suffix uses the framework base name only.
func (f *Framework) CreateNamespace(ctx context.Context, suffix string) string {
	name := f.UniqueName(suffix)
	_, err := client.Namespaces(f.Client).Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	ExpectNoError(err, "create namespace %s", name)
	ginkgo.DeferCleanup(f.DeleteNamespace, name)
	Logf("created namespace %s", name)
	return name
}

// DeleteNamespace deletes ns and waits until it and its contents are gone.
func (f *Framework) DeleteNamespace(ctx context.Context, ns string) {
	err := client.Namespaces(f.Client).Delete(ctx, ns, nil)
	if client.IsNotFound(err) {
		return
	}
	ExpectNoError(err)
	gomega.Eventually(ctx, func() error {
		_, err := client.Namespaces(f.Client).Get(ctx, ns)
		if client.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("namespace %s still exists", ns)
	}).WithTimeout(2 * time.Minute).WithPolling(TestContext.Poll).Should(gomega.Succeed())
}

// UniqueName returns a DNS label such as "e2e-deploy-scale-3fa9c1".
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

// ExpectDefaultNodes fails unless the cluster has exactly the nodes the
// specs assume (Nodes): say, after kk cluster up --workers 3 or with a Node
// object left over by an aborted run.
func ExpectDefaultNodes(ctx context.Context) {
	c := client.New(TestContext.APIURL, TestContext.Token)
	nodes, err := client.Nodes(c).List(ctx, client.ListOptions{})
	ExpectNoError(err)
	var names []string
	for _, n := range nodes.Items {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	gomega.Expect(names).To(gomega.Equal(Nodes),
		"the e2e suite needs the default nodes: run kk cluster up --workers 2 and delete other Node objects")
}

// WaitForClusterReady blocks until the API is healthy and at least workers
// nodes report a fresh Ready heartbeat.
func WaitForClusterReady(ctx context.Context, workers int) {
	c := client.New(TestContext.APIURL, TestContext.Token)
	gomega.Eventually(ctx, func(g gomega.Gomega) {
		g.Expect(c.Health(ctx)).To(gomega.Succeed())
		nodes, err := client.Nodes(c).List(ctx, client.ListOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		ready := 0
		for i := range nodes.Items {
			if corev1.IsNodeReady(&nodes.Items[i]) {
				ready++
			}
		}
		g.Expect(ready).To(gomega.BeNumerically(">=", workers), "Ready workers")
	}).WithTimeout(TestContext.ClusterReadyTimeout).WithPolling(time.Second).Should(gomega.Succeed())
}
