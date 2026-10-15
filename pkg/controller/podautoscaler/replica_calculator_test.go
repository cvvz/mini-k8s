package podautoscaler

import (
	"context"
	"strings"
	"testing"
	"time"

	"mini-k8s/pkg/api/resource"
	autoscalingv2 "mini-k8s/pkg/apis/autoscaling/v2"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	metricsv1beta1 "mini-k8s/pkg/apis/metrics/v1beta1"
	"mini-k8s/pkg/util/ptr"
)

var now = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// fakeMetrics serves fixed Pod metrics: CPU (milli) and memory (bytes)
// per Pod.
type fakeMetrics struct {
	cpu, mem map[string]int64
	err      error
}

func (f fakeMetrics) PodMetrics(context.Context, string, string) ([]metricsv1beta1.PodMetrics, error) {
	var out []metricsv1beta1.PodMetrics
	for name, cpu := range f.cpu {
		out = append(out, metricsv1beta1.PodMetrics{ObjectMeta: metav1.ObjectMeta{Name: name},
			Timestamp: metav1.Time{Time: now.Add(-5 * time.Second)}, Window: metav1.Duration{Duration: 10 * time.Second},
			Containers: []metricsv1beta1.ContainerMetrics{{Name: "c", Usage: corev1.ResourceList{
				corev1.ResourceCPU: resource.NewMilliQuantity(cpu), corev1.ResourceMemory: resource.NewQuantity(f.mem[name])}}}})
	}
	return out, f.err
}

// readyPod has run for an hour, ready since its start, requesting 100m.
func readyPod(name string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("64Mi")}}}}}}
	p.Status.Phase = corev1.PodRunning
	p.Status.StartTime = &metav1.Time{Time: now.Add(-time.Hour)}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue,
		LastTransitionTime: metav1.Time{Time: now.Add(-time.Hour)}}}
	return p
}

func calculator(m MetricsClient) *replicaCalculator {
	return &replicaCalculator{metrics: m, tolerance: defaultTolerance, cpuInitializationPeriod: defaultCPUInitializationPeriod,
		delayOfInitialReadinessStatus: defaultDelayOfInitialReadinessStatus, now: func() time.Time { return now }}
}

func TestResourceReplicas(t *testing.T) {
	ctx := context.Background()
	pods := []*corev1.Pod{readyPod("a"), readyPod("b")}
	for _, tc := range []struct {
		name        string
		cpu         map[string]int64
		pods        []*corev1.Pod
		current     int32
		want        int32
		utilization int32
	}{
		{"scale up", map[string]int64{"a": 200, "b": 200}, pods, 2, 8, 200},
		{"within tolerance", map[string]int64{"a": 52, "b": 54}, pods, 2, 2, 53},
		{"scale down", map[string]int64{"a": 10, "b": 10}, pods, 2, 1, 10},
		// b has no metrics yet: on a scale down it counts as fully used,
		// which here keeps the count.
		{"missing on scale down", map[string]int64{"a": 20}, pods, 2, 2, 20},
		// A pending Pod counts as idle when scaling up.
		{"pending on scale up", map[string]int64{"a": 200}, append([]*corev1.Pod{readyPod("a")}, func() *corev1.Pod {
			p := readyPod("c")
			p.Status.Phase = corev1.PodPending
			return p
		}()), 2, 4, 200},
	} {
		got, utilization, _, _, err := calculator(fakeMetrics{cpu: tc.cpu}).resourceReplicas(ctx, tc.current, 50, "cpu", "ns", "app=x", "", tc.pods)
		if err != nil || got != tc.want || utilization != tc.utilization {
			t.Errorf("%s: %d replicas at %d%% (%v), want %d at %d%%", tc.name, got, utilization, err, tc.want, tc.utilization)
		}
	}

	// A Pod starting up whose sample predates its readiness is ignored.
	starting := readyPod("s")
	starting.Status.StartTime = &metav1.Time{Time: now.Add(-time.Minute)}
	starting.Status.Conditions[0].LastTransitionTime = metav1.Time{Time: now.Add(-2 * time.Second)}
	_, _, _, _, err := calculator(fakeMetrics{cpu: map[string]int64{"s": 500}}).resourceReplicas(ctx, 1, 50, "cpu", "ns", "", "",
		[]*corev1.Pod{starting})
	if err == nil || err.Error() != "did not receive metrics for targeted pods (pods might be unready)" {
		t.Fatalf("starting Pod: %v", err)
	}
	if _, _, _, _, err := calculator(fakeMetrics{}).resourceReplicas(ctx, 1, 50, "cpu", "ns", "", "", pods); err == nil ||
		err.Error() != "unable to get metrics for resource cpu: no metrics returned from resource metrics API" {
		t.Fatalf("no metrics: %v", err)
	}
	noRequest := readyPod("n")
	noRequest.Spec.Containers[0].Resources.Requests = nil
	if _, _, _, _, err := calculator(fakeMetrics{cpu: map[string]int64{"n": 10}}).resourceReplicas(ctx, 1, 50, "cpu", "ns", "", "",
		[]*corev1.Pod{noRequest}); err == nil || err.Error() != "missing request for cpu in container c of Pod n" {
		t.Fatalf("no request: %v", err)
	}
}

func TestRawResourceReplicas(t *testing.T) {
	pods := []*corev1.Pod{readyPod("a"), readyPod("b")}
	m := fakeMetrics{cpu: map[string]int64{"a": 1, "b": 1}, mem: map[string]int64{"a": 150 << 20, "b": 150 << 20}}
	got, usage, _, err := calculator(m).rawResourceReplicas(context.Background(), 2, 100<<20*1000, "memory", "ns", "", "", pods)
	if err != nil || got != 3 || usage != 150<<20*1000 {
		t.Fatalf("memory: %d replicas, usage %d, %v", got, usage, err)
	}
}

func TestConvertDesiredReplicasWithRules(t *testing.T) {
	for _, tc := range []struct {
		current, desired, want int32
		reason                 string
	}{
		{1, 3, 3, "DesiredWithinRange"},
		{1, 20, 4, "ScaleUpLimit"},
		{5, 20, 10, "TooManyReplicas"},
		{3, 0, 1, "TooFewReplicas"},
	} {
		got, reason, _ := convertDesiredReplicasWithRules(tc.current, tc.desired, 1, 10)
		if got != tc.want || reason != tc.reason {
			t.Errorf("%d -> %d: %d %s, want %d %s", tc.current, tc.desired, got, reason, tc.want, tc.reason)
		}
	}
}

func TestBehaviors(t *testing.T) {
	hc := &Controller{recommendations: map[string][]timestampedRecommendation{}, scaleUpEvents: map[string][]timestampedScaleEvent{},
		scaleDownEvents: map[string][]timestampedScaleEvent{}}
	h := &autoscalingv2.HorizontalPodAutoscaler{Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 20,
		Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{}}}
	autoscalingv2.SetDefaultsHorizontalPodAutoscaler(h)
	// Up: at most 4 Pods or 100% per 15s, whichever is more.
	if got := hc.normalizeWithBehaviors(h, "k", 2, 20, 1, now); got != 6 || h.Status.Condition("ScalingLimited").Reason != "ScaleUpLimit" {
		t.Fatalf("scale up from 2: %d %+v", got, h.Status.Conditions)
	}
	hc.storeScaleEvent(h.Spec.Behavior, "k", 2, 6, now)
	// Within the period the budget is spent: 6 = 2 + 4.
	if got := hc.normalizeWithBehaviors(h, "k", 6, 20, 1, now.Add(5*time.Second)); got != 6 {
		t.Fatalf("budget spent: %d", got)
	}
	if got := hc.normalizeWithBehaviors(h, "k", 6, 20, 1, now.Add(20*time.Second)); got != 12 {
		t.Fatalf("next period: %d", got)
	}
	// Down: the highest recommendation of the last 300s holds.
	if got := hc.normalizeWithBehaviors(h, "k", 12, 2, 1, now.Add(30*time.Second)); got != 12 ||
		h.Status.Condition("AbleToScale").Reason != "ScaleDownStabilized" {
		t.Fatalf("stabilized: %d %+v", got, h.Status.Conditions)
	}
	h.Spec.Behavior.ScaleDown.StabilizationWindowSeconds = ptr.To[int32](0)
	h.Spec.Behavior.ScaleDown.Policies = []autoscalingv2.HPAScalingPolicy{{Type: "Pods", Value: 3, PeriodSeconds: 60}}
	if got := hc.normalizeWithBehaviors(h, "k2", 12, 2, 1, now); got != 9 || h.Status.Condition("ScalingLimited").Reason != "ScaleDownLimit" {
		t.Fatalf("scale down 3 per minute: %d %+v", got, h.Status.Conditions)
	}
	h.Spec.Behavior.ScaleUp.SelectPolicy = ptr.To(autoscalingv2.DisabledPolicySelect)
	if got := hc.normalizeWithBehaviors(h, "k3", 2, 10, 1, now); got != 2 {
		t.Fatalf("disabled scale up: %d", got)
	}
}

func TestErrorMessage(t *testing.T) {
	if !strings.Contains(errorMessage(errNoMetrics), "no metrics") {
		t.Fatal(errorMessage(errNoMetrics))
	}
}
