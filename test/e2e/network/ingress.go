package network

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("network", "Ingress", func() {
	f := framework.NewDefaultFramework("ingress")
	// Every spec routes its own host, so specs running in parallel and the
	// README demo (which routes every other host) do not interfere.
	var host string
	BeforeEach(func() { host = f.Namespace + ".example.com" })
	get := func(ctx SpecContext, path string) (int, string, error) {
		return framework.HTTPGetHost(ctx, framework.TestContext.IngressURL+path, host)
	}
	backend := func(name string, labels map[string]string) []framework.Object {
		return []framework.Object{framework.NewDeployment(name, 1, labels, framework.DemoContainer()), framework.NewService(name, labels, 8080)}
	}

	It("should route by host and path to different Services and report its address", func(ctx SpecContext) {
		f.Apply(ctx, append(backend("app", map[string]string{"app": "app"}), backend("api", map[string]string{"app": "api"})...)...)
		f.Apply(ctx, framework.NewIngress("routes", host,
			framework.IngressPath("/", "Prefix", "app", 80),
			framework.IngressPath("/api", "Prefix", "api", 80),
			framework.IngressPath("/exact", "Exact", "api", 80)))
		app := f.WaitForRunningPods(ctx, map[string]string{"app": "app"}, 1)[0]
		api := f.WaitForRunningPods(ctx, map[string]string{"app": "api"}, 1)[0]

		for path, want := range map[string]string{
			"/": app.Name, "/apis": app.Name, "/api": api.Name, "/api/v1/x": api.Name,
			"/exact": api.Name, "/exact/more": app.Name,
		} {
			Eventually(ctx, func() (string, error) {
				_, body, err := get(ctx, path)
				return body, err
			}).WithTimeout(framework.TestContext.PodStartTimeout).Should(Equal("hello from "+want+"\n"), path)
		}
		Eventually(ctx, func() string { return f.KK(ctx, "get", "ingress", "routes") }).Should(
			MatchRegexp(`routes\s+<none>\s+` + strings.ReplaceAll(host, ".", `\.`) + `\s+localhost\s+80`))

		By("ignoring hosts it has no rule for")
		_, body, err := framework.HTTPGetHost(ctx, framework.TestContext.IngressURL+"/api", "other."+host)
		framework.ExpectNoError(err)
		Expect(body).NotTo(ContainSubstring(api.Name), "other hosts never reach this Ingress's backends")
	})

	It("should keep the request path and query", func(ctx SpecContext) {
		labels := map[string]string{"app": "path"}
		f.Apply(ctx, backend("path", labels)...)
		f.Apply(ctx, framework.NewIngress("path", host, framework.IngressPath("/", "Prefix", "path", 80)))
		pod := f.WaitForRunningPods(ctx, labels, 1)[0]
		Eventually(ctx, func() (int, error) {
			code, _, err := get(ctx, "/e2e/path?probe=1")
			return code, err
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(Equal(200))
		Eventually(ctx, func() (string, error) {
			return framework.RunKK(ctx, "logs", pod.Name, "-n", f.Namespace)
		}).Should(ContainSubstring("GET /e2e/path?probe=1"))
	})

	It("should answer 503 while the Service has no ready endpoints", func(ctx SpecContext) {
		f.Apply(ctx, framework.NewService("empty", map[string]string{"app": "empty"}, 8080),
			framework.NewIngress("empty", host, framework.IngressPath("/", "Prefix", "empty", 80)))
		Eventually(ctx, func() (string, error) {
			code, body, err := get(ctx, "/")
			if code != 503 {
				return "", err
			}
			return body, err
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(ContainSubstring(`no endpoints available for service "` + f.Namespace + `/empty"`))
	})

	It("should keep serving through a rolling update", func(ctx SpecContext) {
		labels := map[string]string{"app": "nonstop"}
		version := func(v string) framework.Object {
			c := framework.DemoContainer()
			// Keep serving briefly after SIGTERM while the Ingress drops the Pod.
			c.Env = map[string]string{"VERSION": v, "DEMO_SHUTDOWN_DELAY": "2s"}
			c.Fields = map[string]any{"readinessProbe": map[string]any{
				"httpGet": map[string]any{"path": "/ready", "port": 8080}, "periodSeconds": 1}}
			d := framework.NewDeployment("nonstop", 2, labels, c)
			d.Spec()["strategy"] = map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": 1, "maxUnavailable": 0}}
			return d
		}
		f.Apply(ctx, version("1"), framework.NewService("nonstop", labels, 8080), framework.NewIngress("nonstop", host, framework.IngressPath("/", "Prefix", "nonstop", 80)))
		framework.KK(ctx, "rollout", "-n", f.Namespace, "status", "deployment/nonstop", "--timeout", "90s")
		Eventually(ctx, func() (int, error) {
			code, _, err := get(ctx, "/")
			return code, err
		}).WithTimeout(20 * time.Second).Should(Equal(200))

		f.Apply(ctx, version("2"))
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = framework.RunKK(ctx, "rollout", "-n", f.Namespace, "status", "deployment/nonstop", "--timeout", "90s")
		}()
		requests := 0
		for running := true; running; {
			select {
			case <-done:
				running = false
			case <-time.After(100 * time.Millisecond):
				code, body, err := get(ctx, "/")
				framework.ExpectNoError(err)
				Expect(code).To(Equal(200), "request %d during the rollout: %s", requests, body)
				requests++
			}
		}
		Expect(requests).To(BeNumerically(">", 5))
	})

	It("should route only to Pods whose readiness probe passes", func(ctx SpecContext) {
		labels := map[string]string{"app": "gated"}
		c := framework.DemoContainer()
		c.Fields = map[string]any{"readinessProbe": map[string]any{
			"httpGet": map[string]any{"path": "/ready", "port": 8080}, "periodSeconds": 1, "failureThreshold": 1}}
		f.Apply(ctx, framework.NewDeployment("gated", 1, labels, c), framework.NewService("gated", labels, 8080),
			framework.NewIngress("gated", host, framework.IngressPath("/", "Prefix", "gated", 80)))
		pod := f.WaitForRunningPods(ctx, labels, 1)[0]
		status := func() (int, error) {
			code, _, err := get(ctx, "/")
			return code, err
		}
		Eventually(ctx, status).WithTimeout(20 * time.Second).Should(Equal(200))

		By("failing the readiness probe")
		_, err := f.FetchFromPod(ctx, pod.Name, "http://127.0.0.1:8080/fail-ready")
		framework.ExpectNoError(err)
		Eventually(ctx, status).WithTimeout(20 * time.Second).Should(Equal(503))
		Expect(f.KK(ctx, "get", "pod", pod.Name)).To(MatchRegexp(pod.Name + `\s+0/1\s+Running\s+0`))
		var unhealthy []string
		for _, e := range f.Events(ctx, pod.Name) {
			if e.Reason == "Unhealthy" {
				unhealthy = append(unhealthy, e.Message)
			}
		}
		Expect(unhealthy).To(ContainElement("Readiness probe failed: HTTP probe failed with statuscode: 503"))

		By("recovering")
		_, err = f.FetchFromPod(ctx, pod.Name, "http://127.0.0.1:8080/recover")
		framework.ExpectNoError(err)
		Eventually(ctx, status).WithTimeout(20 * time.Second).Should(Equal(200))
	})

	It("should serve its default backend for unmatched requests and 404 without one", Serial, func(ctx SpecContext) {
		labels := map[string]string{"app": "fallback"}
		f.Apply(ctx, backend("fallback", labels)...)
		pod := f.WaitForRunningPods(ctx, labels, 1)[0]
		ing := framework.NewIngress("fallback", host, framework.IngressPath("/only", "Exact", "fallback", 80))
		ing.Spec()["defaultBackend"] = map[string]any{"service": map[string]any{"name": "fallback", "port": map[string]any{"number": 80}}}
		f.Apply(ctx, ing)
		Eventually(ctx, func() (string, error) {
			_, body, err := get(ctx, "/elsewhere")
			return body, err
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(Equal("hello from " + pod.Name + "\n"))

		framework.KK(ctx, "delete", "-n", f.Namespace, "ingress", "fallback")
		// Rules without a host (such as the README demo's) catch every other
		// host; only without them does the controller answer 404.
		all, err := client.Ingresses(f.Client, "").List(ctx, client.ListOptions{})
		framework.ExpectNoError(err)
		catchAll := false
		for _, ing := range all.Items {
			for _, r := range ing.Spec.Rules {
				catchAll = catchAll || r.Host == ""
			}
			catchAll = catchAll || ing.Spec.DefaultBackend != nil
		}
		Eventually(ctx, func(g Gomega) {
			code, body, err := get(ctx, "/elsewhere")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(body).NotTo(ContainSubstring(pod.Name))
			if !catchAll {
				g.Expect(code).To(Equal(404))
				g.Expect(body).To(Equal("default backend - 404\n"))
			}
		}).WithTimeout(20 * time.Second).Should(Succeed())
	})

	It("should reject invalid Ingresses with Kubernetes' messages", func(ctx SpecContext) {
		bad := framework.NewIngress("bad", "Foo_bar", map[string]any{"path": "api",
			"backend": map[string]any{"service": map[string]any{"name": "web", "port": map[string]any{"number": 80}}}})
		Expect(f.ApplyExpectError(ctx, bad)).To(And(
			ContainSubstring(`spec.rules[0].host: Invalid value: "Foo_bar": a lowercase RFC 1123 subdomain`),
			ContainSubstring("spec.rules[0].http.paths[0].pathType: Required value: pathType must be specified")))
		empty := framework.Object{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": map[string]any{"name": "empty"}, "spec": map[string]any{}}
		Expect(f.ApplyExpectError(ctx, empty)).To(ContainSubstring("spec: Invalid value: null: either `defaultBackend` or `rules` must be specified"))
	})
})
