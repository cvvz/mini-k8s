// Package podautoscaler implements the HorizontalPodAutoscaler controller,
// like k8s.io/kubernetes/pkg/controller/podautoscaler: every sync period it
// reads the target's scale and its Pods' resource metrics, proposes a
// replica count per metric (the highest wins), stabilizes and rate-limits
// it (with the autoscaler's behavior, or the legacy rules) and updates the
// target's scale subresource.
package podautoscaler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"mini-k8s/pkg/api/resource"
	autoscalingv2 "mini-k8s/pkg/apis/autoscaling/v2"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/client/workqueue"
	"mini-k8s/pkg/labels"
	"mini-k8s/pkg/scheme"
	"mini-k8s/pkg/util/deepcopy"
	"mini-k8s/pkg/util/equality"
	"mini-k8s/pkg/util/ptr"
)

// Defaults of kube-controller-manager's HPA flags.
const (
	DefaultSyncPeriod                    = 15 * time.Second
	defaultTolerance                     = 0.1
	defaultCPUInitializationPeriod       = 5 * time.Minute
	defaultDelayOfInitialReadinessStatus = 30 * time.Second
	defaultDownscaleStabilisationWindow  = 5 * time.Minute
	scaleUpLimitFactor                   = 2.0
	scaleUpLimitMinimum                  = 4.0
)

type timestampedRecommendation struct {
	recommendation int32
	timestamp      time.Time
}

type timestampedScaleEvent struct {
	replicaChange int32
	timestamp     time.Time
	outdated      bool
}

// Controller scales the targets of HorizontalPodAutoscalers.
type Controller struct {
	client     *client.Client
	hpas       *informers.Informer[autoscalingv2.HorizontalPodAutoscaler, *autoscalingv2.HorizontalPodAutoscaler]
	pods       *informers.Informer[corev1.Pod, *corev1.Pod]
	recorder   *record.Recorder
	queue      *workqueue.Queue
	calc       *replicaCalculator
	syncPeriod time.Duration
	now        func() time.Time

	mu              sync.Mutex
	recommendations map[string][]timestampedRecommendation
	scaleUpEvents   map[string][]timestampedScaleEvent
	scaleDownEvents map[string][]timestampedScaleEvent
	scheduled       map[string]bool
}

// NewController wires the controller onto shared informers; metrics are
// read through the API server's metrics.k8s.io.
func NewController(c *client.Client, hpas *informers.Informer[autoscalingv2.HorizontalPodAutoscaler, *autoscalingv2.HorizontalPodAutoscaler],
	pods *informers.Informer[corev1.Pod, *corev1.Pod], recorder *record.Recorder, syncPeriod time.Duration) *Controller {
	return newController(c, hpas, pods, recorder, syncPeriod, restMetrics{c})
}

func newController(c *client.Client, hpas *informers.Informer[autoscalingv2.HorizontalPodAutoscaler, *autoscalingv2.HorizontalPodAutoscaler],
	pods *informers.Informer[corev1.Pod, *corev1.Pod], recorder *record.Recorder, syncPeriod time.Duration, mc MetricsClient) *Controller {
	hc := &Controller{client: c, hpas: hpas, pods: pods, recorder: recorder, queue: workqueue.New(), syncPeriod: syncPeriod, now: time.Now,
		recommendations: map[string][]timestampedRecommendation{}, scaleUpEvents: map[string][]timestampedScaleEvent{},
		scaleDownEvents: map[string][]timestampedScaleEvent{}, scheduled: map[string]bool{}}
	hc.calc = &replicaCalculator{metrics: mc, tolerance: defaultTolerance, cpuInitializationPeriod: defaultCPUInitializationPeriod,
		delayOfInitialReadinessStatus: defaultDelayOfInitialReadinessStatus, now: func() time.Time { return hc.now() }}
	hpas.AddHandler(informers.Handler[*autoscalingv2.HorizontalPodAutoscaler]{
		OnAdd: func(h *autoscalingv2.HorizontalPodAutoscaler) { hc.queue.Add(informers.Key(h)) },
		// Status writes do not trigger a sync; spec changes do.
		OnUpdate: func(old, cur *autoscalingv2.HorizontalPodAutoscaler) {
			if !equality.Semantic(old.Spec, cur.Spec) {
				hc.queue.Add(informers.Key(cur))
			}
		},
		OnDelete: func(h *autoscalingv2.HorizontalPodAutoscaler) { hc.queue.Add(informers.Key(h)) },
	})
	return hc
}

// Run processes autoscalers until ctx ends.
func (hc *Controller) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, hc.queue, "horizontal-pod-autoscaler", workers, hc.sync)
}

// requeue schedules the next periodic sync of key, once.
func (hc *Controller) requeue(key string) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if hc.scheduled[key] {
		return
	}
	hc.scheduled[key] = true
	time.AfterFunc(hc.syncPeriod, func() {
		hc.mu.Lock()
		delete(hc.scheduled, key)
		hc.mu.Unlock()
		hc.queue.Add(key)
	})
}

func (hc *Controller) sync(ctx context.Context, key string) error {
	ns, name := informers.SplitKey(key)
	cached, ok := hc.hpas.Get(ns, name)
	if !ok {
		hc.mu.Lock()
		delete(hc.recommendations, key)
		delete(hc.scaleUpEvents, key)
		delete(hc.scaleDownEvents, key)
		hc.mu.Unlock()
		return nil
	}
	defer hc.requeue(key)
	hpa := deepcopy.Copy(cached)
	autoscalingv2.SetDefaultsHorizontalPodAutoscaler(hpa)
	err := hc.reconcile(ctx, key, hpa)
	if !equality.Semantic(hpa.Status, cached.Status) {
		cp := deepcopy.Copy(cached)
		cp.Status = hpa.Status
		if _, uerr := client.HorizontalPodAutoscalers(hc.client, ns).UpdateStatus(ctx, cp); uerr != nil && !client.IsNotFound(uerr) &&
			!client.IsConflict(uerr) {
			return uerr
		}
	}
	_ = err // reported through conditions and events; the next period retries
	return nil
}

// targetResource finds the scaled resource of a scaleTargetRef.
func targetResource(ref autoscalingv2.CrossVersionObjectReference) (scheme.ResourceInfo, error) {
	group, _, _ := strings.Cut(ref.APIVersion, "/")
	if !strings.Contains(ref.APIVersion, "/") {
		group = ""
	}
	for _, r := range scheme.All {
		if r.Kind == ref.Kind && r.Group == group {
			return r, nil
		}
	}
	return scheme.ResourceInfo{}, fmt.Errorf("no matches for kind %q in group %q", ref.Kind, group)
}

func (hc *Controller) reconcile(ctx context.Context, key string, hpa *autoscalingv2.HorizontalPodAutoscaler) error {
	now := hc.now()
	hpa.Status.ObservedGeneration = ptr.To(hpa.Generation)
	ref := hpa.Spec.ScaleTargetRef
	res, err := targetResource(ref)
	var scale autoscalingv2.Scale
	scalePath := ""
	if err == nil {
		scalePath = res.Path(hpa.Namespace, ref.Name) + "/scale"
		err = hc.client.Do(ctx, http.MethodGet, scalePath, nil, "", nil, &scale)
	}
	if err != nil {
		msg := errorMessage(err)
		setCondition(hpa, autoscalingv2.AbleToScale, corev1.ConditionFalse, "FailedGetScale",
			"the HPA controller was unable to get the target's current scale: "+msg, now)
		hc.recorder.Eventf(hpa, corev1.EventTypeWarning, "FailedGetScale", "%s", msg)
		hpa.Status.CurrentMetrics = nil
		return err
	}
	setCondition(hpa, autoscalingv2.AbleToScale, corev1.ConditionTrue, "SucceededGetScale",
		"the HPA controller was able to get the target's current scale", now)
	current := scale.Spec.Replicas
	hc.recordInitialRecommendation(key, current, now)
	minReplicas := *hpa.Spec.MinReplicas
	var statuses []autoscalingv2.MetricStatus
	desired, reason := int32(0), ""
	rescale := true
	switch {
	case current == 0 && minReplicas != 0:
		rescale = false
		setCondition(hpa, autoscalingv2.ScalingActive, corev1.ConditionFalse, "ScalingDisabled",
			"scaling is disabled since the replica count of the target is zero", now)
	case current > hpa.Spec.MaxReplicas:
		desired, reason = hpa.Spec.MaxReplicas, "Current number of replicas above Spec.MaxReplicas"
	case current < minReplicas:
		desired, reason = minReplicas, "Current number of replicas below Spec.MinReplicas"
	default:
		proposal, metric, sts, err := hc.computeReplicasForMetrics(ctx, hpa, &scale, now)
		statuses = sts
		if err != nil && proposal == -1 {
			hpa.Status.CurrentReplicas, hpa.Status.CurrentMetrics = current, statuses
			hc.recorder.Eventf(hpa, corev1.EventTypeWarning, "FailedComputeMetricsReplicas", "%s", err.Error())
			return err
		}
		if hpa.Spec.Behavior == nil {
			desired = hc.normalize(hpa, key, current, proposal, minReplicas, now)
		} else {
			desired = hc.normalizeWithBehaviors(hpa, key, current, proposal, minReplicas, now)
		}
		rescale = desired != current
		switch {
		case desired > current:
			reason = metric + " above target"
		case desired < current:
			reason = "All metrics below target"
		}
	}
	if rescale {
		scale.Spec.Replicas = desired
		if err := hc.client.Do(ctx, http.MethodPut, scalePath, nil, "", &scale, nil); err != nil {
			hc.recorder.Eventf(hpa, corev1.EventTypeWarning, "FailedRescale", "New size: %d; reason: %s; error: %v", desired, reason, errorMessage(err))
			setCondition(hpa, autoscalingv2.AbleToScale, corev1.ConditionFalse, "FailedUpdateScale",
				"the HPA controller was unable to update the target scale: "+errorMessage(err), now)
			hpa.Status.CurrentReplicas, hpa.Status.CurrentMetrics = current, statuses
			return err
		}
		setCondition(hpa, autoscalingv2.AbleToScale, corev1.ConditionTrue, "SucceededRescale",
			fmt.Sprintf("the HPA controller was able to update the target scale to %d", desired), now)
		hc.recorder.Eventf(hpa, corev1.EventTypeNormal, "SuccessfulRescale", "New size: %d; reason: %s", desired, reason)
		hc.storeScaleEvent(hpa.Spec.Behavior, key, current, desired, now)
		hpa.Status.LastScaleTime = &metav1.Time{Time: now}
	} else {
		desired = current
	}
	hpa.Status.CurrentReplicas, hpa.Status.DesiredReplicas, hpa.Status.CurrentMetrics = current, desired, statuses
	return nil
}

// computeReplicasForMetrics takes the highest proposal of the metrics; it
// fails when every metric fails, or when some fail and the rest would
// scale down.
func (hc *Controller) computeReplicasForMetrics(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, scale *autoscalingv2.Scale,
	now time.Time) (int32, string, []autoscalingv2.MetricStatus, error) {
	if scale.Status.Selector == "" {
		msg := "selector is required"
		hc.recorder.Eventf(hpa, corev1.EventTypeWarning, "SelectorRequired", "%s", msg)
		setCondition(hpa, autoscalingv2.ScalingActive, corev1.ConditionFalse, "InvalidSelector",
			"the HPA target's scale is missing a selector", now)
		return -1, "", nil, fmt.Errorf("%s", msg)
	}
	sel, err := labels.Parse(scale.Status.Selector)
	if err != nil {
		setCondition(hpa, autoscalingv2.ScalingActive, corev1.ConditionFalse, "InvalidSelector",
			"the HPA target's scale has an invalid selector: "+err.Error(), now)
		return -1, "", nil, err
	}
	pods := hc.pods.List(hpa.Namespace, sel)
	statuses := make([]autoscalingv2.MetricStatus, len(hpa.Spec.Metrics))
	replicas, metric := int32(0), ""
	invalid := 0
	var firstErr error
	var firstReason, firstMessage string // the condition of the first failure
	for i, spec := range hpa.Spec.Metrics {
		proposal, name, reason, message, err := hc.computeReplicasForMetric(ctx, hpa, spec, scale, pods, &statuses[i])
		if err != nil {
			if invalid == 0 {
				firstErr, firstReason, firstMessage = err, reason, message
			}
			invalid++
			continue
		}
		if replicas == 0 || proposal > replicas {
			replicas, metric = proposal, name
		}
	}
	if firstErr != nil {
		firstErr = fmt.Errorf("invalid metrics (%d invalid out of %d), first error is: %v", invalid, len(hpa.Spec.Metrics), firstErr)
	}
	if invalid >= len(hpa.Spec.Metrics) || (invalid > 0 && replicas < scale.Spec.Replicas) {
		setCondition(hpa, autoscalingv2.ScalingActive, corev1.ConditionFalse, firstReason, firstMessage, now)
		return -1, "", statuses, firstErr
	}
	setCondition(hpa, autoscalingv2.ScalingActive, corev1.ConditionTrue, "ValidMetricFound",
		"the HPA was able to successfully calculate a replica count from "+metric, now)
	return replicas, metric, statuses, firstErr
}

// computeReplicasForMetric proposes a count for one metric and fills its
// status; on failure it returns the reason and message of the
// ScalingActive condition, and the error for the event.
func (hc *Controller) computeReplicasForMetric(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, spec autoscalingv2.MetricSpec,
	scale *autoscalingv2.Scale, pods []*corev1.Pod, status *autoscalingv2.MetricStatus) (int32, string, string, string, error) {
	var name, container, reason, kind string
	var target autoscalingv2.MetricTarget
	switch {
	case spec.Type == autoscalingv2.ResourceMetricSourceType && spec.Resource != nil:
		name, target, reason, kind = spec.Resource.Name, spec.Resource.Target, "FailedGetResourceMetric", "resource"
	case spec.Type == autoscalingv2.ContainerResourceMetricSourceType && spec.ContainerResource != nil:
		name, container, target = spec.ContainerResource.Name, spec.ContainerResource.Container, spec.ContainerResource.Target
		reason, kind = "FailedGetContainerResourceMetric", "container resource"
	default:
		err := fmt.Errorf("unknown metric source type %q", spec.Type)
		return 0, "", "InvalidMetricSourceType", "the HPA was unable to compute the replica count: " + err.Error(), err
	}
	resourceName := name
	fail := func(err error) (int32, string, string, string, error) {
		hc.recorder.Eventf(hpa, corev1.EventTypeWarning, reason, "%s", err.Error())
		what := name + " resource metric value"
		if container != "" {
			what = container + " container metric value"
		}
		return 0, "", reason, "the HPA was unable to compute the replica count: " + err.Error(), fmt.Errorf("failed to get %s: %v", what, err)
	}
	current := scale.Spec.Replicas
	var value autoscalingv2.MetricValueStatus
	var proposal int32
	var metricName string
	if target.AverageValue != nil {
		p, usage, _, err := hc.calc.rawResourceReplicas(ctx, current, target.AverageValue.MilliValue(), resourceName, hpa.Namespace,
			scale.Status.Selector, container, pods)
		if err != nil {
			return fail(fmt.Errorf("failed to get %s usage: %v", name, err))
		}
		proposal, metricName = p, name+" "+kind
		value.AverageValue = ptr.To(resource.NewMilliQuantity(usage))
	} else {
		if target.AverageUtilization == nil {
			return fail(fmt.Errorf("invalid resource metric source: neither an average utilization target nor an average value (usage) target was set"))
		}
		p, utilization, raw, _, err := hc.calc.resourceReplicas(ctx, current, *target.AverageUtilization, resourceName, hpa.Namespace,
			scale.Status.Selector, container, pods)
		if err != nil {
			return fail(fmt.Errorf("failed to get %s utilization: %v", name, err))
		}
		proposal, metricName = p, fmt.Sprintf("%s %s utilization (percentage of request)", name, kind)
		value.AverageUtilization, value.AverageValue = ptr.To(utilization), ptr.To(resource.NewMilliQuantity(raw))
	}
	status.Type = spec.Type
	if container != "" {
		status.ContainerResource = &autoscalingv2.ContainerResourceMetricStatus{Name: name, Container: container, Current: value}
	} else {
		status.Resource = &autoscalingv2.ResourceMetricStatus{Name: name, Current: value}
	}
	return proposal, metricName, "", "", nil
}

func (hc *Controller) recordInitialRecommendation(key string, current int32, now time.Time) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if hc.recommendations[key] == nil {
		hc.recommendations[key] = []timestampedRecommendation{{current, now}}
	}
}

// normalize applies the legacy rules: the highest recommendation of the
// downscale stabilization window, at most double (or 4) per step, and the
// bounds.
func (hc *Controller) normalize(hpa *autoscalingv2.HorizontalPodAutoscaler, key string, current, proposal, minReplicas int32,
	now time.Time) int32 {
	stabilized := hc.stabilize(key, proposal, now)
	if stabilized != proposal {
		setCondition(hpa, autoscalingv2.AbleToScale, corev1.ConditionTrue, "ScaleDownStabilized",
			"recent recommendations were higher than current one, applying the highest recent recommendation", now)
	} else {
		setCondition(hpa, autoscalingv2.AbleToScale, corev1.ConditionTrue, "ReadyForNewScale", "recommended size matches current size", now)
	}
	desired, reason, message := convertDesiredReplicasWithRules(current, stabilized, minReplicas, hpa.Spec.MaxReplicas)
	status := corev1.ConditionTrue
	if desired == stabilized {
		status = corev1.ConditionFalse
	}
	setCondition(hpa, autoscalingv2.ScalingLimited, status, reason, message, now)
	return desired
}

func (hc *Controller) stabilize(key string, proposal int32, now time.Time) int32 {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	highest := proposal
	cutoff := now.Add(-defaultDownscaleStabilisationWindow)
	old := -1
	for i, rec := range hc.recommendations[key] {
		if rec.timestamp.Before(cutoff) {
			old = i
		} else if rec.recommendation > highest {
			highest = rec.recommendation
		}
	}
	if old >= 0 {
		hc.recommendations[key][old] = timestampedRecommendation{proposal, now}
	} else {
		hc.recommendations[key] = append(hc.recommendations[key], timestampedRecommendation{proposal, now})
	}
	return highest
}

func convertDesiredReplicasWithRules(current, desired, minReplicas, maxReplicas int32) (int32, string, string) {
	maximum, reason, message := maxReplicas, "TooManyReplicas", "the desired replica count is more than the maximum replica count"
	if limit := int32(math.Max(scaleUpLimitFactor*float64(current), scaleUpLimitMinimum)); maxReplicas > limit {
		maximum, reason, message = limit, "ScaleUpLimit", "the desired replica count is increasing faster than the maximum scale rate"
	}
	switch {
	case desired < minReplicas:
		return minReplicas, "TooFewReplicas", "the desired replica count is less than the minimum replica count"
	case desired > maximum:
		return maximum, reason, message
	}
	return desired, "DesiredWithinRange", "the desired count is within the acceptable range"
}

// normalizeWithBehaviors applies the autoscaler's behavior: the
// stabilization windows of both directions, then the scaling policies.
func (hc *Controller) normalizeWithBehaviors(hpa *autoscalingv2.HorizontalPodAutoscaler, key string, current, proposal, minReplicas int32,
	now time.Time) int32 {
	b := hpa.Spec.Behavior
	stabilized, reason, message := hc.stabilizeWithBehaviors(key, b, current, proposal, now)
	if stabilized != proposal {
		setCondition(hpa, autoscalingv2.AbleToScale, corev1.ConditionTrue, reason, message, now)
	} else {
		setCondition(hpa, autoscalingv2.AbleToScale, corev1.ConditionTrue, "ReadyForNewScale", "recommended size matches current size", now)
	}
	desired, reason, message := hc.convertWithBehaviorRate(key, b, current, stabilized, minReplicas, hpa.Spec.MaxReplicas, now)
	status := corev1.ConditionTrue
	if desired == stabilized {
		status = corev1.ConditionFalse
	}
	setCondition(hpa, autoscalingv2.ScalingLimited, status, reason, message, now)
	return desired
}

func (hc *Controller) stabilizeWithBehaviors(key string, b *autoscalingv2.HorizontalPodAutoscalerBehavior, current, proposal int32,
	now time.Time) (int32, string, string) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	up, down := proposal, proposal
	upCutoff := now.Add(-time.Duration(*b.ScaleUp.StabilizationWindowSeconds) * time.Second)
	downCutoff := now.Add(-time.Duration(*b.ScaleDown.StabilizationWindowSeconds) * time.Second)
	old := -1
	for i, rec := range hc.recommendations[key] {
		if rec.timestamp.After(upCutoff) {
			up = min(rec.recommendation, up)
		}
		if rec.timestamp.After(downCutoff) {
			down = max(rec.recommendation, down)
		}
		if rec.timestamp.Before(upCutoff) && rec.timestamp.Before(downCutoff) {
			old = i
		}
	}
	recommendation := current
	if recommendation < up {
		recommendation = up
	}
	if recommendation > down {
		recommendation = down
	}
	if old >= 0 {
		hc.recommendations[key][old] = timestampedRecommendation{proposal, now}
	} else {
		hc.recommendations[key] = append(hc.recommendations[key], timestampedRecommendation{proposal, now})
	}
	if proposal >= current {
		return recommendation, "ScaleUpStabilized", "recent recommendations were lower than current one, applying the lowest recent recommendation"
	}
	return recommendation, "ScaleDownStabilized", "recent recommendations were higher than current one, applying the highest recent recommendation"
}

func (hc *Controller) convertWithBehaviorRate(key string, b *autoscalingv2.HorizontalPodAutoscalerBehavior, current, desired, minReplicas,
	maxReplicas int32, now time.Time) (int32, string, string) {
	hc.mu.Lock()
	ups, downs := hc.scaleUpEvents[key], hc.scaleDownEvents[key]
	hc.mu.Unlock()
	switch {
	case desired > current:
		limit := max(scaleUpLimit(current, ups, downs, b.ScaleUp, now), current)
		maximum, reason, message := maxReplicas, "TooManyReplicas", "the desired replica count is more than the maximum replica count"
		if maximum > limit {
			maximum, reason, message = limit, "ScaleUpLimit", "the desired replica count is increasing faster than the maximum scale rate"
		}
		if desired > maximum {
			return maximum, reason, message
		}
	case desired < current:
		limit := min(scaleDownLimit(current, ups, downs, b.ScaleDown, now), current)
		minimum, reason, message := minReplicas, "TooFewReplicas", "the desired replica count is less than the minimum replica count"
		if minimum < limit {
			minimum, reason, message = limit, "ScaleDownLimit", "the desired replica count is decreasing faster than the maximum scale rate"
		}
		if desired < minimum {
			return minimum, reason, message
		}
	}
	return desired, "DesiredWithinRange", "the desired count is within the acceptable range"
}

// replicasChangedInPeriod sums the changes of the events of the last
// period.
func replicasChangedInPeriod(periodSeconds int32, events []timestampedScaleEvent, now time.Time) int32 {
	cutoff := now.Add(-time.Duration(periodSeconds) * time.Second)
	var n int32
	for _, e := range events {
		if e.timestamp.After(cutoff) {
			n += e.replicaChange
		}
	}
	return n
}

func scaleUpLimit(current int32, ups, downs []timestampedScaleEvent, r *autoscalingv2.HPAScalingRules, now time.Time) int32 {
	if *r.SelectPolicy == autoscalingv2.DisabledPolicySelect {
		return current
	}
	pick, result := func(a, b int32) int32 { return max(a, b) }, int32(math.MinInt32)
	if *r.SelectPolicy == autoscalingv2.MinChangePolicySelect {
		pick, result = func(a, b int32) int32 { return min(a, b) }, math.MaxInt32
	}
	for _, p := range r.Policies {
		start := current - replicasChangedInPeriod(p.PeriodSeconds, ups, now) + replicasChangedInPeriod(p.PeriodSeconds, downs, now)
		var proposed int32
		if p.Type == autoscalingv2.PodsScalingPolicy {
			proposed = start + p.Value
		} else {
			proposed = int32(math.Ceil(float64(start) * (1 + float64(p.Value)/100)))
		}
		result = pick(result, proposed)
	}
	return result
}

func scaleDownLimit(current int32, ups, downs []timestampedScaleEvent, r *autoscalingv2.HPAScalingRules, now time.Time) int32 {
	if *r.SelectPolicy == autoscalingv2.DisabledPolicySelect {
		return current
	}
	pick, result := func(a, b int32) int32 { return min(a, b) }, int32(math.MaxInt32)
	if *r.SelectPolicy == autoscalingv2.MinChangePolicySelect {
		pick, result = func(a, b int32) int32 { return max(a, b) }, math.MinInt32
	}
	for _, p := range r.Policies {
		start := current - replicasChangedInPeriod(p.PeriodSeconds, ups, now) + replicasChangedInPeriod(p.PeriodSeconds, downs, now)
		var proposed int32
		if p.Type == autoscalingv2.PodsScalingPolicy {
			proposed = start - p.Value
		} else {
			proposed = int32(float64(start) * (1 - float64(p.Value)/100))
		}
		result = pick(result, proposed)
	}
	return result
}

// storeScaleEvent records a change for the scaling policies; events older
// than the longest policy period are reused.
func (hc *Controller) storeScaleEvent(b *autoscalingv2.HorizontalPodAutoscalerBehavior, key string, prev, next int32, now time.Time) {
	if b == nil {
		return
	}
	hc.mu.Lock()
	defer hc.mu.Unlock()
	rules, events := b.ScaleUp, hc.scaleUpEvents
	change := next - prev
	if next < prev {
		rules, events, change = b.ScaleDown, hc.scaleDownEvents, prev-next
	}
	var longest int32
	for _, p := range rules.Policies {
		longest = max(longest, p.PeriodSeconds)
	}
	cutoff := now.Add(-time.Duration(longest) * time.Second)
	list := events[key]
	for i := range list {
		if list[i].timestamp.Before(cutoff) {
			list[i] = timestampedScaleEvent{replicaChange: change, timestamp: now}
			events[key] = list
			return
		}
	}
	events[key] = append(list, timestampedScaleEvent{replicaChange: change, timestamp: now})
}

// setCondition sets a condition, moving its transition time only when its
// status changes.
func setCondition(hpa *autoscalingv2.HorizontalPodAutoscaler, typ, status, reason, message string, now time.Time) {
	if c := hpa.Status.Condition(typ); c != nil {
		if c.Status != status {
			c.LastTransitionTime = metav1.Time{Time: now}
		}
		c.Status, c.Reason, c.Message = status, reason, message
		return
	}
	hpa.Status.Conditions = append(hpa.Status.Conditions, autoscalingv2.HorizontalPodAutoscalerCondition{Type: typ, Status: status,
		Reason: reason, Message: message, LastTransitionTime: metav1.Time{Time: now}})
}

// errorMessage is an API error's message, as Kubernetes prints it.
func errorMessage(err error) string {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Status.Message != "" {
		return apiErr.Status.Message
	}
	return err.Error()
}
