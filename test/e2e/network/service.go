// Package network holds Service and gateway specs.
package network

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("network", "Service gateway", func() {
	f := framework.NewDefaultFramework("svc")

	// v1 supports exactly one Service; never touch one owned by the user.
	BeforeEach(func(ctx SpecContext) {
		services, err := f.Client.List(ctx, "Service")
		framework.ExpectNoError(err)
		if len(services) > 0 {
			Skip("cluster already has Service " + services[0].Metadata.Name + "; v1 supports a single Service")
		}
	})

	It("should load-balance requests across all running backends", func(ctx SpecContext) {
		name := f.UniqueName("lb")
		labels := map[string]string{"app": name}
		f.Apply(ctx,
			framework.NewDeployment(name, 2, labels, framework.DemoContainer()),
			framework.NewService(name, labels, 8080))
		pods := f.WaitForRunningPods(ctx, labels, 2)

		hosts := map[string]bool{}
		for _, p := range pods {
			hosts[p.Status.ContainerID[:12]] = false
		}
		Eventually(ctx, func(g Gomega) {
			code, body, err := framework.HTTPGet(ctx, framework.TestContext.GatewayURL+"/")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(code).To(Equal(200), body)
			host := strings.TrimSpace(strings.TrimPrefix(body, "hello from "))
			g.Expect(hosts).To(HaveKey(host))
			hosts[host] = true
			for h, seen := range hosts {
				g.Expect(seen).To(BeTrue(), "no response from backend %s yet", h)
			}
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(Succeed())
	})

	It("should preserve the request path and query", func(ctx SpecContext) {
		name := f.UniqueName("path")
		labels := map[string]string{"app": name}
		f.Apply(ctx,
			framework.NewDeployment(name, 1, labels, framework.DemoContainer()),
			framework.NewService(name, labels, 8080))
		pod := f.WaitForRunningPods(ctx, labels, 1)[0]

		Eventually(ctx, func() (int, error) {
			code, _, err := framework.HTTPGet(ctx, framework.TestContext.GatewayURL+"/e2e/path?probe=1")
			return code, err
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(Equal(200))
		Eventually(ctx, func() (string, error) {
			return framework.RunKK(ctx, "logs", pod.Metadata.Name)
		}).Should(ContainSubstring("GET /e2e/path"))
	})

	It("should return 503 while the Service has no running backends", func(ctx SpecContext) {
		name := f.UniqueName("empty")
		f.Apply(ctx, framework.NewService(name, map[string]string{"app": name}, 8080))
		Eventually(ctx, func() (int, error) {
			code, _, err := framework.HTTPGet(ctx, framework.TestContext.GatewayURL+"/")
			return code, err
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(Equal(503))
	})

	It("should reject a second Service with a clear error", func(ctx SpecContext) {
		first := f.UniqueName("first")
		f.Apply(ctx, framework.NewService(first, map[string]string{"app": first}, 8080))
		out := f.ApplyExpectError(ctx, framework.NewService(f.UniqueName("second"), map[string]string{"app": "x"}, 8080))
		Expect(out).To(ContainSubstring("only one Service is supported"))
	})
})
