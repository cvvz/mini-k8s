// Package e2e is the mini-k8s end-to-end suite. Specs live in per-area
// packages (apps, node, network, storage, apimachinery, lifecycle) and register
// themselves through blank imports, like Kubernetes' test/e2e.
//
// The suite mutates the running project cluster and therefore only runs with
// MINI_E2E=1; see `make e2e`.
package e2e

import (
	"os"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"mini-k8s/test/e2e/framework"

	_ "mini-k8s/test/e2e/apimachinery"
	_ "mini-k8s/test/e2e/apps"
	_ "mini-k8s/test/e2e/controlplane"
	_ "mini-k8s/test/e2e/instrumentation"
	_ "mini-k8s/test/e2e/lifecycle"
	_ "mini-k8s/test/e2e/network"
	_ "mini-k8s/test/e2e/node"
	_ "mini-k8s/test/e2e/scheduling"
	_ "mini-k8s/test/e2e/storage"
)

func TestE2E(t *testing.T) {
	if os.Getenv("MINI_E2E") != "1" {
		t.Skip("set MINI_E2E=1 (or run make e2e) against a running, disposable project cluster")
	}
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "mini-k8s e2e suite")
}

var _ = ginkgo.BeforeSuite(func(ctx ginkgo.SpecContext) {
	framework.ExpectNoError(framework.Setup())
	framework.WaitForClusterReady(ctx, len(framework.Nodes))
	framework.ExpectDefaultNodes(ctx)
})
