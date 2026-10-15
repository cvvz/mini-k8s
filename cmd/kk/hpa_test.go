package main

import (
	"context"
	"strings"
	"testing"

	"mini-k8s/pkg/api/resource"
	appsv1 "mini-k8s/pkg/apis/apps/v1"
	autoscalingv2 "mini-k8s/pkg/apis/autoscaling/v2"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/test/integration/framework"
)

func TestHPATargets(t *testing.T) {
	util := func(name string, target int32) autoscalingv2.MetricSpec {
		return autoscalingv2.MetricSpec{Type: "Resource", Resource: &autoscalingv2.ResourceMetricSource{Name: name,
			Target: autoscalingv2.MetricTarget{Type: "Utilization", AverageUtilization: ptr.To(target)}}}
	}
	avg := autoscalingv2.MetricSpec{Type: "Resource", Resource: &autoscalingv2.ResourceMetricSource{Name: "memory",
		Target: autoscalingv2.MetricTarget{Type: "AverageValue", AverageValue: ptr.To(resource.MustParse("100Mi"))}}}
	h := &autoscalingv2.HorizontalPodAutoscaler{Spec: autoscalingv2.HorizontalPodAutoscalerSpec{Metrics: []autoscalingv2.MetricSpec{util("cpu", 50)}}}
	if got := hpaTargets(h); got != "cpu: <unknown>/50%" {
		t.Errorf("unknown: %q", got)
	}
	h.Status.CurrentMetrics = []autoscalingv2.MetricStatus{{Type: "Resource", Resource: &autoscalingv2.ResourceMetricStatus{Name: "cpu",
		Current: autoscalingv2.MetricValueStatus{AverageUtilization: ptr.To[int32](201)}}}}
	if got := hpaTargets(h); got != "cpu: 201%/50%" {
		t.Errorf("current: %q", got)
	}
	h.Spec.Metrics = append(h.Spec.Metrics, avg, util("cpu", 90))
	if got := hpaTargets(h); got != "cpu: 201%/50%, memory: <unknown>/100Mi + 1 more..." {
		t.Errorf("three metrics: %q", got)
	}
}

func TestAutoscale(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := context.Background()
	labels := map[string]string{"app": "web"}
	if _, err := client.Deployments(c, "default").Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"autoscale", "deployment", "web"}, "--max=MAXPODS is required and must be at least 1, max: -1"},
		{[]string{"autoscale", "deployment", "web", "--max", "1", "--min", "3"}, "--max=MAXPODS must be larger or equal to --min=MINPODS, max: 1, min: 3"},
		{[]string{"autoscale", "deployment", "ghost", "--max", "2"}, `deployments.apps "ghost" not found`},
		{[]string{"autoscale", "pod", "web", "--max", "2"}, "cannot autoscale a Pod"},
		{[]string{"autoscale", "deployment", "web", "--max", "2", "--cpu", "lots"}, `invalid CPU target "lots"`},
	} {
		if _, err := kk(t, c, "", tc.argv...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.argv, err, tc.want)
		}
	}
	if out, err := kk(t, c, "", "autoscale", "deployment/web", "--min", "2", "--max", "5", "--cpu-percent", "60"); err != nil ||
		out != "horizontalpodautoscaler.autoscaling/web autoscaled\n" {
		t.Fatalf("autoscale: %q %v", out, err)
	}
	h, err := client.HorizontalPodAutoscalers(c, "default").Get(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if h.Spec.ScaleTargetRef != (autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "web"}) ||
		*h.Spec.MinReplicas != 2 || h.Spec.MaxReplicas != 5 || *h.Spec.Metrics[0].Resource.Target.AverageUtilization != 60 {
		t.Fatalf("hpa: %+v", h.Spec)
	}
	if out, err := kk(t, c, "", "autoscale", "deployment", "web", "--max", "3", "--cpu", "200m", "--name", "by-value"); err != nil ||
		out != "horizontalpodautoscaler.autoscaling/by-value autoscaled\n" {
		t.Fatalf("by value: %q %v", out, err)
	}
	if _, err := kk(t, c, "", "autoscale", "deployment", "web", "--max", "3"); err == nil || !client.IsAlreadyExists(err) {
		t.Fatalf("duplicate: %v", err)
	}
	out, err := kk(t, c, "", "get", "hpa")
	rows := strings.Split(strings.TrimSpace(out), "\n")
	if err != nil || len(rows) != 3 || strings.Join(strings.Fields(rows[0]), " ") != "NAME REFERENCE TARGETS MINPODS MAXPODS REPLICAS AGE" ||
		!strings.HasPrefix(strings.Join(strings.Fields(rows[1]), " "), "by-value Deployment/web cpu: <unknown>/200m 1 3 0") ||
		!strings.HasPrefix(strings.Join(strings.Fields(rows[2]), " "), "web Deployment/web cpu: <unknown>/60% 2 5 0") {
		t.Fatalf("get hpa: %v\n%s", err, out)
	}
	out, err = kk(t, c, "", "describe", "hpa", "web")
	for _, want := range []string{"Reference:", "Deployment/web", "resource cpu on pods  (as a percentage of request):", "<unknown> / 60%",
		"Min replicas:", "Deployment pods:", "0 current / 0 desired"} {
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("describe lacks %q: %v\n%s", want, err, out)
		}
	}
}
