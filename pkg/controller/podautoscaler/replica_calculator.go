package podautoscaler

import (
	"context"
	"fmt"
	"math"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// replicaCalculator proposes replica counts from Pod metrics, like
// Kubernetes' ReplicaCalculator: Pods that are pending, starting up or
// missing metrics are counted conservatively so they do not cause a
// scale in the wrong direction.
type replicaCalculator struct {
	metrics   MetricsClient
	tolerance float64
	// cpuInitializationPeriod is how long after start a Pod's CPU is
	// trusted only once it is ready; delayOfInitialReadinessStatus how long
	// a Pod may stay unready at start without being ignored later.
	cpuInitializationPeriod       time.Duration
	delayOfInitialReadinessStatus time.Duration
	now                           func() time.Time
}

// resourceReplicas proposes a count for a target utilization (a
// percentage of the requests); it also returns the current utilization
// and the average raw usage (milli-units).
func (c *replicaCalculator) resourceReplicas(ctx context.Context, currentReplicas, targetUtilization int32, resource string,
	namespace, selector, container string, pods []*corev1.Pod) (int32, int32, int64, time.Time, error) {
	metrics, timestamp, err := resourceMetric(ctx, c.metrics, resource, namespace, selector, container)
	if err != nil {
		return 0, 0, 0, time.Time{}, fmt.Errorf("unable to get metrics for resource %s: %v", resource, err)
	}
	if len(pods) == 0 {
		return 0, 0, 0, time.Time{}, fmt.Errorf("no pods returned by selector while calculating replica count")
	}
	ready, unready, missing, ignored := c.groupPods(pods, metrics, resource)
	removeMetrics(metrics, ignored)
	removeMetrics(metrics, unready)
	requests, err := podRequests(pods, container, resource)
	if err != nil {
		return 0, 0, 0, time.Time{}, err
	}
	if len(metrics) == 0 {
		return 0, 0, 0, time.Time{}, fmt.Errorf("did not receive metrics for targeted pods (pods might be unready)")
	}
	usageRatio, utilization, rawAverage, err := utilizationRatio(metrics, requests, targetUtilization)
	if err != nil {
		return 0, 0, 0, time.Time{}, err
	}
	scaleUpWithUnready := len(unready) > 0 && usageRatio > 1.0
	if !scaleUpWithUnready && len(missing) == 0 {
		if math.Abs(1.0-usageRatio) <= c.tolerance {
			return currentReplicas, utilization, rawAverage, timestamp, nil
		}
		return int32(math.Ceil(usageRatio * float64(ready))), utilization, rawAverage, timestamp, nil
	}
	// Assume the worst for Pods without a usable metric: full use of their
	// request when scaling down, none when scaling up.
	for name := range missing {
		if usageRatio < 1.0 {
			metrics[name] = podMetric{Value: max(requests[name], requests[name]*int64(targetUtilization)/100)}
		} else {
			metrics[name] = podMetric{Value: 0}
		}
	}
	if scaleUpWithUnready {
		for name := range unready {
			metrics[name] = podMetric{Value: 0}
		}
	}
	newRatio, _, _, err := utilizationRatio(metrics, requests, targetUtilization)
	if err != nil {
		return 0, utilization, rawAverage, time.Time{}, err
	}
	if math.Abs(1.0-newRatio) <= c.tolerance || (usageRatio < 1.0 && newRatio > 1.0) || (usageRatio > 1.0 && newRatio < 1.0) {
		return currentReplicas, utilization, rawAverage, timestamp, nil
	}
	newReplicas := int32(math.Ceil(newRatio * float64(len(metrics))))
	if (newRatio < 1.0 && newReplicas > currentReplicas) || (newRatio > 1.0 && newReplicas < currentReplicas) {
		return currentReplicas, utilization, rawAverage, timestamp, nil
	}
	return newReplicas, utilization, rawAverage, timestamp, nil
}

// rawResourceReplicas proposes a count for a target average usage per Pod
// (milli-units); it also returns the current average.
func (c *replicaCalculator) rawResourceReplicas(ctx context.Context, currentReplicas int32, targetUsage int64, resource string,
	namespace, selector, container string, pods []*corev1.Pod) (int32, int64, time.Time, error) {
	metrics, timestamp, err := resourceMetric(ctx, c.metrics, resource, namespace, selector, container)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("unable to get metrics for resource %s: %v", resource, err)
	}
	if len(pods) == 0 {
		return 0, 0, time.Time{}, fmt.Errorf("no pods returned by selector while calculating replica count")
	}
	ready, unready, missing, ignored := c.groupPods(pods, metrics, resource)
	removeMetrics(metrics, ignored)
	removeMetrics(metrics, unready)
	if len(metrics) == 0 {
		return 0, 0, time.Time{}, fmt.Errorf("did not receive metrics for targeted pods (pods might be unready)")
	}
	average := func() int64 {
		var sum int64
		for _, m := range metrics {
			sum += m.Value
		}
		return sum / int64(len(metrics))
	}
	usage := average()
	usageRatio := float64(usage) / float64(targetUsage)
	scaleUpWithUnready := len(unready) > 0 && usageRatio > 1.0
	if !scaleUpWithUnready && len(missing) == 0 {
		if math.Abs(1.0-usageRatio) <= c.tolerance {
			return currentReplicas, usage, timestamp, nil
		}
		return int32(math.Ceil(usageRatio * float64(ready))), usage, timestamp, nil
	}
	for name := range missing {
		if usageRatio < 1.0 {
			metrics[name] = podMetric{Value: targetUsage}
		} else {
			metrics[name] = podMetric{Value: 0}
		}
	}
	if scaleUpWithUnready {
		for name := range unready {
			metrics[name] = podMetric{Value: 0}
		}
	}
	newRatio := float64(average()) / float64(targetUsage)
	if math.Abs(1.0-newRatio) <= c.tolerance || (usageRatio < 1.0 && newRatio > 1.0) || (usageRatio > 1.0 && newRatio < 1.0) {
		return currentReplicas, usage, timestamp, nil
	}
	newReplicas := int32(math.Ceil(newRatio * float64(len(metrics))))
	if (newRatio < 1.0 && newReplicas > currentReplicas) || (newRatio > 1.0 && newReplicas < currentReplicas) {
		return currentReplicas, usage, timestamp, nil
	}
	return newReplicas, usage, timestamp, nil
}

type nameSet map[string]bool

// groupPods sorts Pods into ready ones (counted), unready ones (pending,
// or starting up as far as CPU goes), those without metrics and ignored
// ones (deleted or failed).
func (c *replicaCalculator) groupPods(pods []*corev1.Pod, metrics podMetricsInfo, resource string) (
	ready int, unready, missing, ignored nameSet) {
	unready, missing, ignored = nameSet{}, nameSet{}, nameSet{}
	now := c.now()
	for _, p := range pods {
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			ignored[p.Name] = true
			continue
		}
		if p.Status.Phase == corev1.PodPending {
			unready[p.Name] = true
			continue
		}
		metric, found := metrics[p.Name]
		if !found {
			missing[p.Name] = true
			continue
		}
		if resource == corev1.ResourceCPU {
			cond := p.Status.Condition(corev1.PodReady)
			isUnready := false
			switch {
			case cond == nil || p.Status.StartTime == nil:
				isUnready = true
			case p.Status.StartTime.Add(c.cpuInitializationPeriod).After(now):
				// Starting up: trust only samples taken after it became ready.
				isUnready = cond.Status == corev1.ConditionFalse || metric.Timestamp.Before(cond.LastTransitionTime.Add(metric.Window))
			default:
				// Ignore Pods unready since their start.
				isUnready = cond.Status == corev1.ConditionFalse && p.Status.StartTime.Add(c.delayOfInitialReadinessStatus).After(cond.LastTransitionTime.Time)
			}
			if isUnready {
				unready[p.Name] = true
				continue
			}
		}
		ready++
	}
	return ready, unready, missing, ignored
}

func removeMetrics(metrics podMetricsInfo, names nameSet) {
	for name := range names {
		delete(metrics, name)
	}
}

// podRequests sums each Pod's requests of a resource (of one container,
// if given); a container without a request makes utilization undefined.
func podRequests(pods []*corev1.Pod, container string, resource string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, p := range pods {
		var sum int64
		for _, c := range p.Spec.Containers {
			if container != "" && c.Name != container {
				continue
			}
			q, ok := c.Resources.Requests[resource]
			if !ok {
				return nil, fmt.Errorf("missing request for %s in container %s of Pod %s", resource, c.Name, p.Name)
			}
			sum += q.MilliValue()
		}
		out[p.Name] = sum
	}
	return out, nil
}

// utilizationRatio compares the usage of the Pods with requests to the
// target: usage/requests in percent, divided by the target percentage.
func utilizationRatio(metrics podMetricsInfo, requests map[string]int64, target int32) (float64, int32, int64, error) {
	var metricsTotal, requestsTotal int64
	entries := 0
	for name, m := range metrics {
		request, ok := requests[name]
		if !ok {
			continue
		}
		metricsTotal += m.Value
		requestsTotal += request
		entries++
	}
	if requestsTotal == 0 {
		return 0, 0, 0, fmt.Errorf("no metrics returned matched known pods")
	}
	utilization := int32(metricsTotal * 100 / requestsTotal)
	return float64(utilization) / float64(target), utilization, metricsTotal / int64(entries), nil
}
