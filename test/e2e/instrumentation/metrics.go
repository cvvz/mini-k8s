// Package instrumentation holds the [sig-instrumentation] e2e specs: the
// kubelet's resource metrics, metrics-server and the metrics API.
package instrumentation

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metricsv1beta1 "mini-k8s/pkg/apis/metrics/v1beta1"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("instrumentation", "Metrics", func() {
	f := framework.NewDefaultFramework("metrics")

	// milliCores reads the CPU column of a `kk top` row.
	milliCores := func(row string) int {
		m := regexp.MustCompile(`\s(\d+)m\s`).FindStringSubmatch(row + " ")
		if m == nil {
			return -1
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	podRow := func(ctx context.Context, name string) string {
		out, err := framework.RunKK(ctx, "top", "pod", name, "-n", f.Namespace)
		if err != nil {
			return ""
		}
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, name+" ") {
				return l
			}
		}
		return ""
	}

	It("should report node and Pod usage through kk top and the metrics API", func(ctx SpecContext) {
		By("listing every node")
		Eventually(ctx, func() string { out, _ := framework.RunKK(ctx, "top", "nodes"); return out }).WithTimeout(time.Minute).
			Should(MatchRegexp(`(?s)NAME\s+CPU\(cores\)\s+CPU\(%\)\s+MEMORY\(bytes\)\s+MEMORY\(%\).*worker-1\s+\d+m\s+\d+%\s+\d+Mi\s+\d+%`))
		nodes, err := framework.RunningNodes(ctx)
		framework.ExpectNoError(err)
		out := framework.KK(ctx, "top", "nodes")
		for _, n := range nodes {
			Expect(out).To(MatchRegexp(`(?m)^` + n + `\s+\d+m\s+`))
		}

		By("measuring a Pod that keeps half a core busy")
		spinner := framework.MiniContainer("spin", "spin")
		spinner.Fields = map[string]any{"resources": map[string]any{
			"requests": map[string]any{"cpu": "100m", "memory": "32Mi"}, "limits": map[string]any{"cpu": "500m", "memory": "64Mi"}}}
		hog := framework.MiniContainer("hog", "alloc", "48Mi")
		f.Apply(ctx, framework.NewPod("busy", map[string]string{"app": "busy"}, spinner).WithContainers(spinner, hog))
		f.WaitForRunningPods(ctx, map[string]string{"app": "busy"}, 1)
		Eventually(ctx, func() int { return milliCores(podRow(ctx, "busy")) }).WithTimeout(90*time.Second).WithPolling(2*time.Second).
			Should(BeNumerically("~", 500, 150), "the CPU limit caps the spinner at 500m")
		out = framework.KK(ctx, "top", "pod", "busy", "--containers", "-n", f.Namespace)
		Expect(out).To(MatchRegexp(`(?m)^busy\s+spin\s+\d+m\s+\d+Mi`))
		Expect(out).To(MatchRegexp(`(?m)^busy\s+hog\s+\d+m\s+(4[89]|5\d)Mi`), "the hog holds about 48Mi")

		By("reading the PodMetrics object the API server proxies from metrics-server")
		var m metricsv1beta1.PodMetrics
		framework.ExpectNoError(f.Client.Do(ctx, http.MethodGet, "/apis/metrics.k8s.io/v1beta1/namespaces/"+f.Namespace+"/pods/busy",
			nil, "", nil, &m))
		Expect(m.Kind).To(Equal("PodMetrics"))
		Expect(m.Labels).To(HaveKeyWithValue("app", "busy"))
		Expect(m.Window.Duration).To(BeNumerically(">", 0))
		Expect(m.Containers).To(HaveLen(2))
		var list metricsv1beta1.PodMetricsList
		framework.ExpectNoError(f.Client.Do(ctx, http.MethodGet, "/apis/metrics.k8s.io/v1beta1/namespaces/"+f.Namespace+"/pods",
			map[string][]string{"labelSelector": {"app=other"}}, "", nil, &list))
		Expect(list.Items).To(BeEmpty())

		By("sorting Pods by CPU")
		f.Apply(ctx, framework.NewPod("idle", map[string]string{"app": "idle"}, framework.MiniContainer("idle", "sleep", "1h")))
		Eventually(ctx, func() []string {
			out, _ := framework.RunKK(ctx, "top", "pods", "--sort-by", "cpu", "-n", f.Namespace)
			var names []string
			for _, l := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
				names = append(names, strings.Fields(l)[0])
			}
			return names
		}).WithTimeout(90 * time.Second).Should(Equal([]string{"busy", "idle"}))
	})

	It("should serve the kubelet's resource metrics through the nodes/proxy subresource", func(ctx SpecContext) {
		f.Apply(ctx, framework.NewPod("probe", map[string]string{"app": "probe"}, framework.MiniContainer("app", "sleep", "1h")))
		pod := f.WaitForRunningPods(ctx, map[string]string{"app": "probe"}, 1)[0]
		var body string
		Eventually(ctx, func() (string, error) {
			var raw []byte
			err := f.Client.Do(ctx, http.MethodGet, "/api/v1/nodes/"+pod.Spec.NodeName+"/proxy/metrics/resource", nil, "", nil, &raw)
			body = string(raw)
			return body, err
		}).WithTimeout(30 * time.Second).Should(ContainSubstring(`container_cpu_usage_seconds_total{container="app",namespace="` +
			f.Namespace + `",pod="probe"}`))
		for _, family := range []string{"node_cpu_usage_seconds_total ", "node_memory_working_set_bytes ",
			`pod_memory_working_set_bytes{namespace="` + f.Namespace + `",pod="probe"}`, "resource_scrape_error 0"} {
			Expect(body).To(ContainSubstring(family))
		}
	})

	It("should answer ServiceUnavailable while metrics-server is down", framework.WithDisruptive(), func(ctx SpecContext) {
		DeferCleanup(func(ctx SpecContext) {
			framework.KK(ctx, "scale", "deployment", "metrics-server", "-n", "kube-system", "--replicas", "1")
			Eventually(ctx, func() string { out, _ := framework.RunKK(ctx, "top", "nodes"); return out }).
				WithTimeout(2*time.Minute).Should(MatchRegexp(`(?m)^worker-1\s+\d+m`), "metrics are back for later specs")
		})
		framework.KK(ctx, "scale", "deployment", "metrics-server", "-n", "kube-system", "--replicas", "0")
		Eventually(ctx, func() string { out, _ := framework.RunKK(ctx, "top", "nodes"); return out }).WithTimeout(time.Minute).
			Should(ContainSubstring("Metrics API not available"))
		out, err := framework.RunKK(ctx, "api-resources")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("deployments"))
		Expect(out).To(ContainSubstring("unable to retrieve the complete list of server APIs: metrics.k8s.io/v1beta1: " +
			"the server is currently unable to handle the request"))
	})
})
