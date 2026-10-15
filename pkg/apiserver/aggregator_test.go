package apiserver_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
)

func TestMetricsAPIIsProxiedToMetricsServer(t *testing.T) {
	c, _ := newServerWithKubelet(t)
	ctx := context.Background()
	var groups struct {
		Groups []struct {
			Name string `json:"name"`
		} `json:"groups"`
	}
	if err := c.Do(ctx, http.MethodGet, "/apis", nil, "", nil, &groups); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range groups.Groups {
		found = found || g.Name == "metrics.k8s.io"
	}
	if !found {
		t.Fatalf("discovery lacks metrics.k8s.io: %+v", groups)
	}
	var group map[string]any
	if err := c.Do(ctx, http.MethodGet, "/apis/metrics.k8s.io", nil, "", nil, &group); err != nil || group["name"] != "metrics.k8s.io" {
		t.Fatalf("group document: %v %v", group, err)
	}

	// Without a metrics-server endpoint the group is unavailable.
	err := c.Do(ctx, http.MethodGet, "/apis/metrics.k8s.io/v1beta1/nodes", nil, "", nil, nil)
	if client.Reason(err) != metav1.ReasonServiceUnavail || !strings.Contains(err.Error(), "the server is currently unable to handle the request") {
		t.Fatalf("no endpoint: %v", err)
	}

	var seen []string
	ms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization")+" "+r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"kind":"NodeMetricsList","apiVersion":"metrics.k8s.io/v1beta1","metadata":{},"items":[]}`)
	}))
	t.Cleanup(ms.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(ms.URL, "http://"))
	p, _ := strconv.Atoi(port)
	if _, err := client.Endpoints(c, "kube-system").Create(ctx, &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "metrics-server"},
		Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: host}}, Ports: []corev1.EndpointPort{{Port: int32(p)}}}}}); err != nil {
		t.Fatal(err)
	}
	var list map[string]any
	if err := c.Do(ctx, http.MethodGet, "/apis/metrics.k8s.io/v1beta1/nodes", map[string][]string{"labelSelector": {"a=b"}}, "", nil, &list); err != nil ||
		list["kind"] != "NodeMetricsList" {
		t.Fatalf("proxied list: %v %v", list, err)
	}
	if len(seen) != 1 || seen[0] != "Bearer secret /apis/metrics.k8s.io/v1beta1/nodes?labelSelector=a%3Db" {
		t.Fatalf("metrics-server saw %v", seen)
	}
	if err := c.Do(ctx, http.MethodPost, "/apis/metrics.k8s.io/v1beta1/nodes", nil, "", nil, nil); client.Reason(err) != metav1.ReasonMethodNotAllowed {
		t.Fatalf("the metrics API is read-only: %v", err)
	}
}

func TestNodeProxyReachesTheKubelet(t *testing.T) {
	c, kubelet := newServerWithKubelet(t)
	ctx := context.Background()
	var body json.RawMessage
	err := c.Do(ctx, http.MethodGet, "/api/v1/nodes/worker-1/proxy/metrics/resource", nil, "", nil, &body)
	if got := kubelet.last(); got != "GET /metrics/resource?" {
		t.Fatalf("kubelet saw %q (%v)", got, err)
	}
	if err := c.Do(ctx, http.MethodGet, "/api/v1/nodes/missing/proxy/metrics/resource", nil, "", nil, nil); !client.IsNotFound(err) {
		t.Fatalf("missing node: %v", err)
	}
}
