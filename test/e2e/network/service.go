// Package network holds Service and gateway specs.
package network

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mini-k8s/internal/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("network", "Service gateway", Serial, func() {
	f := framework.NewDefaultFramework("svc")

	// The gateway serves exactly one Service cluster-wide; never touch one
	// owned by the user.
	BeforeEach(func(ctx SpecContext) {
		services, err := client.Services(f.Client, "").List(ctx, client.ListOptions{})
		framework.ExpectNoError(err)
		if len(services.Items) > 0 {
			s := services.Items[0]
			Skip("cluster already has Service " + s.Namespace + "/" + s.Name + "; the gateway supports a single Service")
		}
	})

	It("should load-balance requests across all ready backends", func(ctx SpecContext) {
		labels := map[string]string{"app": "lb"}
		f.Apply(ctx,
			framework.NewDeployment("lb", 2, labels, framework.DemoContainer()),
			framework.NewService("lb", labels, 8080))
		pods := f.WaitForRunningPods(ctx, labels, 2)

		hosts := map[string]bool{}
		for _, p := range pods {
			hosts[p.Name] = false
		}
		Eventually(ctx, func(g Gomega) {
			code, body, err := framework.HTTPGet(ctx, framework.TestContext.GatewayURL+"/")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(code).To(Equal(200), body)
			host := strings.TrimSpace(strings.TrimPrefix(body, "hello from "))
			g.Expect(hosts).To(HaveKey(host), "the Pod hostname is its name")
			hosts[host] = true
			for h, seen := range hosts {
				g.Expect(seen).To(BeTrue(), "no response from backend %s yet", h)
			}
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(Succeed())
	})

	It("should preserve the request path and query", func(ctx SpecContext) {
		labels := map[string]string{"app": "path"}
		f.Apply(ctx,
			framework.NewDeployment("path", 1, labels, framework.DemoContainer()),
			framework.NewService("path", labels, 8080))
		pod := f.WaitForRunningPods(ctx, labels, 1)[0]

		Eventually(ctx, func() (int, error) {
			code, _, err := framework.HTTPGet(ctx, framework.TestContext.GatewayURL+"/e2e/path?probe=1")
			return code, err
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(Equal(200))
		Eventually(ctx, func() (string, error) {
			return framework.RunKK(ctx, "logs", pod.Name, "-n", f.Namespace)
		}).Should(ContainSubstring("GET /e2e/path?probe=1"))
	})

	It("should return 503 while the Service has no ready backends", func(ctx SpecContext) {
		f.Apply(ctx, framework.NewService("empty", map[string]string{"app": "empty"}, 8080))
		Eventually(ctx, func() (int, error) {
			code, _, err := framework.HTTPGet(ctx, framework.TestContext.GatewayURL+"/")
			return code, err
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(Equal(503))
	})

	It("should reject a second Service with a clear error", func(ctx SpecContext) {
		f.Apply(ctx, framework.NewService("first", map[string]string{"app": "first"}, 8080))
		out := f.ApplyExpectError(ctx, framework.NewService("second", map[string]string{"app": "x"}, 8080))
		Expect(out).To(ContainSubstring("only one Service is supported"))
	})
})
