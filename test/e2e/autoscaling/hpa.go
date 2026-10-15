// Package autoscaling holds the [sig-autoscaling] e2e specs: the scale
// subresource and the HorizontalPodAutoscaler.
package autoscaling

import (
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	autoscalingv2 "mini-k8s/pkg/apis/autoscaling/v2"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("autoscaling", "HorizontalPodAutoscaler", func() {
	f := framework.NewDefaultFramework("hpa")

	// worker runs `mini ARGS` with a 100m CPU request, capped at 200m.
	worker := func(args ...string) framework.Container {
		c := framework.MiniContainer("work", args...)
		c.Fields = map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": "100m"}, "limits": map[string]any{"cpu": "200m"}}}
		return c
	}
	hpa := func(name string) framework.Object {
		return framework.Object{"apiVersion": "autoscaling/v2", "kind": "HorizontalPodAutoscaler", "metadata": map[string]any{"name": name},
			"spec": map[string]any{
				"scaleTargetRef": map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": name},
				"minReplicas":    1, "maxReplicas": 3,
				"metrics": []any{map[string]any{"type": "Resource", "resource": map[string]any{"name": "cpu",
					"target": map[string]any{"type": "Utilization", "averageUtilization": 50}}}},
				// Scale down as soon as the load is gone (the default waits 5 minutes).
				"behavior": map[string]any{"scaleDown": map[string]any{"stabilizationWindowSeconds": 0}},
			}}
	}
	replicas := func(ctx SpecContext, name string) int32 {
		d, err := client.Deployments(f.Client, f.Namespace).Get(ctx, name)
		framework.ExpectNoError(err)
		return *d.Spec.Replicas
	}

	It("should scale a Deployment up under CPU load and back down once it is idle", framework.WithSlow(), func(ctx SpecContext) {
		labels := map[string]string{"app": "busy"}
		f.Apply(ctx, framework.NewDeployment("busy", 1, labels, worker("spin")), hpa("busy"))

		By("scaling up: each Pod uses about 200% of its request against a 50% target")
		Eventually(ctx, func() int32 { return replicas(ctx, "busy") }).WithTimeout(3 * time.Minute).WithPolling(2 * time.Second).Should(BeEquivalentTo(3))
		f.WaitForRunningPods(ctx, labels, 3)
		Eventually(ctx, func() string { return f.KK(ctx, "get", "hpa", "busy") }).WithTimeout(time.Minute).
			Should(MatchRegexp(`(?m)^busy\s+Deployment/busy\s+cpu: \d{3}%/50%\s+1\s+3\s+3\s+`))
		Expect(f.Events(ctx, "busy")).To(ContainElement(HaveField("Message",
			"New size: 3; reason: cpu resource utilization (percentage of request) above target")))
		h, err := client.HorizontalPodAutoscalers(f.Client, f.Namespace).Get(ctx, "busy")
		framework.ExpectNoError(err)
		Expect(h.Status.Condition(autoscalingv2.ScalingLimited)).To(And(HaveField("Status", "True"), HaveField("Reason", "TooManyReplicas")))
		Expect(h.Status.Condition(autoscalingv2.ScalingActive)).To(HaveField("Reason", "ValidMetricFound"))
		Expect(f.KK(ctx, "describe", "hpa", "busy")).To(MatchRegexp(`resource cpu on pods  \(as a percentage of request\):\s+\d+% \(\d+m\) / 50%`))

		By("removing the load: the autoscaler goes back to the minimum")
		idle := framework.NewDeployment("busy", 1, labels, worker("sleep", "1h"))
		delete(idle.Spec(), "replicas") // the autoscaler owns the count
		f.Apply(ctx, idle)
		Eventually(ctx, func() int32 { return replicas(ctx, "busy") }).WithTimeout(4 * time.Minute).WithPolling(2 * time.Second).Should(BeEquivalentTo(1))
		Eventually(ctx, func() []string {
			var messages []string
			for _, e := range f.Events(ctx, "busy") {
				messages = append(messages, e.Message)
			}
			return messages
		}).Should(ContainElement("New size: 1; reason: All metrics below target"))
		Eventually(ctx, func() string { return f.KK(ctx, "get", "hpa", "busy") }).WithTimeout(time.Minute).
			Should(MatchRegexp(`(?m)^busy\s+Deployment/busy\s+cpu: \d%/50%\s+1\s+3\s+1\s+`))
	})

	It("should create autoscalers with kk autoscale and report targets it cannot scale", framework.WithSlow(), func(ctx SpecContext) {
		f.Apply(ctx, framework.NewDeployment("web", 2, map[string]string{"app": "web"}, worker("sleep", "1h")))
		Expect(f.KK(ctx, "autoscale", "deployment", "web", "--min", "2", "--max", "4", "--cpu-percent", "70")).
			To(Equal("horizontalpodautoscaler.autoscaling/web autoscaled\n"))
		out, err := framework.RunKK(ctx, "autoscale", "deployment", "web", "--max", "1", "--min", "3", "-n", f.Namespace)
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("--max=MAXPODS must be larger or equal to --min=MINPODS, max: 1, min: 3"))
		Eventually(ctx, func() string { return f.KK(ctx, "get", "hpa", "web") }).WithTimeout(time.Minute).
			Should(MatchRegexp(`(?m)^web\s+Deployment/web\s+cpu: \d+%/70%\s+2\s+4\s+2\s+`))

		By("reading the scale subresource the autoscaler uses")
		var scale autoscalingv2.Scale
		framework.ExpectNoError(f.Client.Do(ctx, http.MethodGet, "/apis/apps/v1/namespaces/"+f.Namespace+"/deployments/web/scale",
			nil, "", nil, &scale))
		Expect(scale.Spec.Replicas).To(BeEquivalentTo(2))
		Expect(scale.Status.Selector).To(Equal("app=web"))

		By("an autoscaler of a missing Deployment")
		ghost := hpa("ghost")
		f.Apply(ctx, ghost)
		Eventually(ctx, func() string { return f.KK(ctx, "describe", "hpa", "ghost") }).WithTimeout(time.Minute).Should(MatchRegexp(
			`AbleToScale\s+False\s+FailedGetScale\s+the HPA controller was unable to get the target's current scale: deployments.apps "ghost" not found`))
		Expect(f.Events(ctx, "ghost")).To(ContainElement(HaveField("Reason", "FailedGetScale")))

		By("rejecting invalid autoscalers")
		bad := hpa("bad")
		bad["spec"].(map[string]any)["minReplicas"] = 5
		Expect(f.ApplyExpectError(ctx, bad)).To(ContainSubstring("spec.maxReplicas: Invalid value: 3: must be greater than or equal to `minReplicas`"))
	})
})
