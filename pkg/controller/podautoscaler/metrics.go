package podautoscaler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	metricsv1beta1 "mini-k8s/pkg/apis/metrics/v1beta1"
	"mini-k8s/pkg/client"
)

// podMetric is one Pod's usage of a resource, in milli-units.
type podMetric struct {
	Timestamp time.Time
	Window    time.Duration
	Value     int64
}

// podMetricsInfo maps Pod names to their usage.
type podMetricsInfo map[string]podMetric

// MetricsClient reads the resource metrics API.
type MetricsClient interface {
	// PodMetrics lists the metrics of the Pods a selector matches.
	PodMetrics(ctx context.Context, namespace, selector string) ([]metricsv1beta1.PodMetrics, error)
}

// restMetrics reads metrics.k8s.io through the API server.
type restMetrics struct{ client *client.Client }

func (m restMetrics) PodMetrics(ctx context.Context, namespace, selector string) ([]metricsv1beta1.PodMetrics, error) {
	var list metricsv1beta1.PodMetricsList
	err := m.client.Do(ctx, http.MethodGet, "/apis/"+metricsv1beta1.GroupVersion+"/namespaces/"+namespace+"/pods",
		url.Values{"labelSelector": {selector}}, "", nil, &list)
	return list.Items, err
}

var errNoMetrics = errors.New("no metrics returned from resource metrics API")

// resourceMetric returns each Pod's usage of a resource (the sum over its
// containers, or one container's), like the HPA's metrics client.
func resourceMetric(ctx context.Context, mc MetricsClient, resource string, namespace, selector, container string) (
	podMetricsInfo, time.Time, error) {
	items, err := mc.PodMetrics(ctx, namespace, selector)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("unable to fetch metrics from resource metrics API: %v", err)
	}
	if len(items) == 0 {
		return nil, time.Time{}, errNoMetrics
	}
	out := podMetricsInfo{}
	for _, m := range items {
		var sum int64
		found := false
		for _, c := range m.Containers {
			if container != "" && c.Name != container {
				continue
			}
			q, ok := c.Usage[resource]
			if !ok {
				found = false
				break
			}
			sum += q.MilliValue()
			found = true
		}
		if found {
			out[m.Name] = podMetric{Timestamp: m.Timestamp.Time, Window: m.Window.Duration, Value: sum}
		}
	}
	if len(out) == 0 {
		return nil, time.Time{}, errNoMetrics
	}
	return out, items[0].Timestamp.Time, nil
}
