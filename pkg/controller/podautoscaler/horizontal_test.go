package podautoscaler

import (
	"context"
	"sync"
	"testing"
	"time"

	"mini-k8s/pkg/api/resource"
	appsv1 "mini-k8s/pkg/apis/apps/v1"
	autoscalingv2 "mini-k8s/pkg/apis/autoscaling/v2"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	metricsv1beta1 "mini-k8s/pkg/apis/metrics/v1beta1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/test/integration/framework"
)

// liveMetrics reports the same CPU usage for every Pod the test lists.
type liveMetrics struct {
	mu    sync.Mutex
	cpu   int64
	pods  func() []string
	stamp func() time.Time
}

func (m *liveMetrics) set(cpu int64) {
	m.mu.Lock()
	m.cpu = cpu
	m.mu.Unlock()
}

func (m *liveMetrics) PodMetrics(context.Context, string, string) ([]metricsv1beta1.PodMetrics, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []metricsv1beta1.PodMetrics
	for _, name := range m.pods() {
		out = append(out, metricsv1beta1.PodMetrics{ObjectMeta: metav1.ObjectMeta{Name: name}, Timestamp: metav1.Time{Time: m.stamp()},
			Window: metav1.Duration{Duration: 10 * time.Second}, Containers: []metricsv1beta1.ContainerMetrics{{Name: "c",
				Usage: corev1.ResourceList{corev1.ResourceCPU: resource.NewMilliQuantity(m.cpu)}}}})
	}
	return out, nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestControllerScalesADeployment(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	labels := map[string]string{"app": "web"}
	if _, err := client.Deployments(c, "default").Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1), Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}}}); err != nil {
		t.Fatal(err)
	}
	replicas := func() int32 {
		d, err := client.Deployments(c, "default").Get(ctx, "web")
		if err != nil {
			return -1
		}
		return *d.Spec.Replicas
	}
	// Stand in for the ReplicaSet controller: as many ready Pods as the
	// Deployment wants, ready for an hour.
	pods := informers.New(client.Pods(c, ""), client.ListOptions{}, 0)
	go pods.Run(ctx)
	go func() {
		for ctx.Err() == nil {
			n := int(replicas())
			list := pods.List("default", nil)
			for i := len(list); i < n; i++ {
				p := readyPod("")
				p.GenerateName, p.Labels = "web-", labels
				p.Spec.Containers[0].Image = "img"
				if created, err := client.Pods(c, "default").Create(ctx, p); err == nil {
					// Ready since an hour ago, by the controller's clock.
					hourAgo := metav1.Time{Time: time.Now().Add(-time.Hour)}
					created.Status = readyPod("").Status
					created.Status.StartTime, created.Status.Conditions[0].LastTransitionTime = &hourAgo, hourAgo
					_, _ = client.Pods(c, "default").UpdateStatus(ctx, created)
				}
			}
			for i := n; i < len(list); i++ {
				_ = client.Pods(c, "default").Delete(ctx, list[i].Name, &metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	m := &liveMetrics{cpu: 200, stamp: time.Now, pods: func() []string {
		var names []string
		for _, p := range pods.List("default", nil) {
			if p.Status.Phase == corev1.PodRunning {
				names = append(names, p.Name)
			}
		}
		return names
	}}
	rec := record.New(c, "test", "")
	go rec.Run(ctx)
	hpas := informers.New(client.HorizontalPodAutoscalers(c, ""), client.ListOptions{}, 0)
	go hpas.Run(ctx)
	informers.WaitForSync(ctx, hpas.HasSynced, pods.HasSynced)
	go newController(c, hpas, pods, rec, 200*time.Millisecond, m).Run(ctx, 1)

	h := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
		ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "web"}, MaxReplicas: 3,
		Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{ScaleDown: &autoscalingv2.HPAScalingRules{
			StabilizationWindowSeconds: ptr.To[int32](0)}}}}
	created, err := client.HorizontalPodAutoscalers(c, "default").Create(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if *created.Spec.MinReplicas != 1 || *created.Spec.Metrics[0].Resource.Target.AverageUtilization != 80 ||
		*created.Spec.Behavior.ScaleDown.StabilizationWindowSeconds != 0 || *created.Spec.Behavior.ScaleUp.StabilizationWindowSeconds != 0 {
		t.Fatalf("defaults: %+v", created.Spec)
	}
	get := func() *autoscalingv2.HorizontalPodAutoscaler {
		h, _ := client.HorizontalPodAutoscalers(c, "default").Get(ctx, "web")
		return h
	}
	hasEvent := func(reason, message string) bool {
		events, _ := client.Events(c, "default").List(ctx, client.ListOptions{})
		for _, e := range events.Items {
			if e.InvolvedObject.Name == "web" && e.Reason == reason && e.Message == message {
				return true
			}
		}
		return false
	}

	// 200m of 100m requested is 200% against 80%: 1 * 2.5 = 3 Pods.
	defer func() {
		if t.Failed() {
			t.Logf("autoscaler status at failure: %+v; replicas %d", get().Status, replicas())
		}
	}()
	waitFor(t, "the scale up", func() bool { return replicas() == 3 })
	waitFor(t, "the status", func() bool {
		s := get().Status
		return s.CurrentReplicas == 3 && s.DesiredReplicas == 3 && len(s.CurrentMetrics) == 1 && s.LastScaleTime != nil &&
			*s.CurrentMetrics[0].Resource.Current.AverageUtilization == 200 && s.CurrentMetrics[0].Resource.Current.AverageValue.String() == "200m"
	})
	s := get().Status
	if c := s.Condition(autoscalingv2.ScalingActive); c.Status != "True" || c.Reason != "ValidMetricFound" ||
		c.Message != "the HPA was able to successfully calculate a replica count from cpu resource utilization (percentage of request)" {
		t.Fatalf("ScalingActive: %+v", c)
	}
	if c := s.Condition(autoscalingv2.ScalingLimited); c.Status != "True" || c.Reason != "TooManyReplicas" {
		t.Fatalf("ScalingLimited: %+v", c)
	}
	waitFor(t, "the rescale event", func() bool {
		return hasEvent("SuccessfulRescale", "New size: 3; reason: cpu resource utilization (percentage of request) above target")
	})

	// Idle Pods: back to the minimum.
	m.set(5)
	waitFor(t, "the scale down", func() bool { return replicas() == 1 })
	waitFor(t, "the scale down event", func() bool { return hasEvent("SuccessfulRescale", "New size: 1; reason: All metrics below target") })

	// No metrics at all: ScalingActive goes False.
	m.mu.Lock()
	m.pods = func() []string { return nil }
	m.mu.Unlock()
	waitFor(t, "the metrics failure", func() bool {
		c := get().Status.Condition(autoscalingv2.ScalingActive)
		return c != nil && c.Status == "False" && c.Reason == "FailedGetResourceMetric" && c.Message == "the HPA was unable to compute the "+
			"replica count: failed to get cpu utilization: unable to get metrics for resource cpu: no metrics returned from resource metrics API"
	})
	waitFor(t, "the failure events", func() bool {
		return hasEvent("FailedComputeMetricsReplicas", "invalid metrics (1 invalid out of 1), first error is: failed to get cpu resource "+
			"metric value: failed to get cpu utilization: unable to get metrics for resource cpu: no metrics returned from resource metrics API")
	})

	// A missing target cannot be scaled.
	ghost := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "ghost"}, Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
		ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "ghost"}, MaxReplicas: 2}}
	if _, err := client.HorizontalPodAutoscalers(c, "default").Create(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "FailedGetScale", func() bool {
		g, err := client.HorizontalPodAutoscalers(c, "default").Get(ctx, "ghost")
		if err != nil {
			return false
		}
		cond := g.Status.Condition(autoscalingv2.AbleToScale)
		return cond != nil && cond.Status == "False" && cond.Reason == "FailedGetScale" &&
			cond.Message == `the HPA controller was unable to get the target's current scale: deployments.apps "ghost" not found`
	})
}
