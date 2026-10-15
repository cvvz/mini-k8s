package metricsserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	metricsv1beta1 "mini-k8s/pkg/apis/metrics/v1beta1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/test/integration/framework"
)

// resourceMetrics renders a kubelet response with the given cumulative
// CPU seconds at ms.
func resourceMetrics(ms int64, nodeCPU, appCPU float64) string {
	return fmt.Sprintf(`# HELP container_cpu_usage_seconds_total [STABLE] Cumulative cpu time consumed by the container in core-seconds
# TYPE container_cpu_usage_seconds_total counter
container_cpu_usage_seconds_total{container="app",namespace="ns",pod="web"} %g %d
container_cpu_usage_seconds_total{container="side,car",namespace="ns",pod="web"} 1 %d
container_memory_working_set_bytes{container="app",namespace="ns",pod="web"} 1.048576e+07 %d
container_memory_working_set_bytes{container="side,car",namespace="ns",pod="web"} 2048 %d
container_start_time_seconds{container="app",namespace="ns",pod="web"} 1.792e+09 %d
node_cpu_usage_seconds_total %g %d
node_memory_working_set_bytes 1.4e+09 %d
resource_scrape_error 0
`, appCPU, ms, ms, ms, ms, ms, nodeCPU, ms, ms)
}

func TestParseAndRates(t *testing.T) {
	s := newStorage()
	b := newBatch()
	if err := parse(strings.NewReader(resourceMetrics(1_000_000, 100, 10)), "w1", b); err != nil {
		t.Fatal(err)
	}
	if p := b.pods[podKey{"ns", "web"}]["side,car"]; p.cpu != 1 || p.mem != 2048 {
		t.Fatalf("a comma inside a label value: %+v", b.pods)
	}
	s.store(b)
	if _, ok := s.node("w1"); ok {
		t.Fatal("one sample gives no rate")
	}
	// Ten seconds later: the node used 5 cores, the app half a core.
	b = newBatch()
	_ = parse(strings.NewReader(resourceMetrics(1_010_000, 150, 15)), "w1", b)
	s.store(b)
	u, ok := s.node("w1")
	if !ok || u.nanoCores != 5e9 || u.memory != 1.4e9 || u.window != 10*time.Second || u.at != time.UnixMilli(1_010_000) {
		t.Fatalf("node usage: %+v %v", u, ok)
	}
	containers, ok := s.pod("ns", "web")
	if !ok || len(containers) != 2 {
		t.Fatalf("pod usage: %+v %v", containers, ok)
	}
	for _, c := range containers {
		if c.name == "app" && (c.nanoCores != 5e8 || c.memory != 10485760) {
			t.Fatalf("app: %+v", c)
		}
	}
	// A restart (CPU going back) gives no rate until the next sample.
	b = newBatch()
	_ = parse(strings.NewReader(resourceMetrics(1_020_000, 160, 1)), "w1", b)
	s.store(b)
	if _, ok := s.pod("ns", "web"); ok {
		t.Fatal("no rate across a restart")
	}
	// What a scrape no longer reports is forgotten.
	s.store(newBatch())
	if _, ok := s.node("w1"); ok {
		t.Fatal("a node missing from the scrape is forgotten")
	}
	if _, _, _, _, err := parseSample("broken{"); err == nil {
		t.Fatal("bad samples are errors")
	}
}

func TestScrapeAndServe(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var calls int
	kubelet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.URL.Path != "/metrics/resource" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		calls++
		fmt.Fprint(w, resourceMetrics(int64(1_000_000+10_000*calls), float64(100+50*calls), float64(10+5*calls)))
	}))
	t.Cleanup(kubelet.Close)
	host, port, _ := strings.Cut(strings.TrimPrefix(kubelet.URL, "http://"), ":")
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1", Labels: map[string]string{"pool": "a"}}}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: host}}
	created, err := client.Nodes(c).Create(ctx, node)
	if err != nil {
		t.Fatal(err)
	}
	created.Status = node.Status
	if _, err := client.Nodes(c).UpdateStatus(ctx, created); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Namespaces(c).Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Pods(c, "ns").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Labels: map[string]string{"app": "web"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "img"}}}}); err != nil {
		t.Fatal(err)
	}
	var p int
	fmt.Sscan(port, &p)
	s := New(c, "secret", p, time.Hour)
	go s.nodes.Run(ctx)
	go s.pods.Run(ctx)
	informers.WaitForSync(ctx, s.nodes.HasSynced, s.pods.HasSynced)
	for s.nodes.List("", nil)[0].InternalIP() == "" {
		time.Sleep(10 * time.Millisecond)
	}
	api := httptest.NewServer(s.Handler())
	t.Cleanup(api.Close)
	get := func(path string, into any) int {
		req, _ := http.NewRequest(http.MethodGet, api.URL+path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if into != nil {
			_ = json.NewDecoder(resp.Body).Decode(into)
		}
		return resp.StatusCode
	}
	if code := get("/readyz", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("ready before a scrape: %d", code)
	}
	s.scrapeAll(ctx)
	var status metav1.Status
	if code := get("/apis/metrics.k8s.io/v1beta1/nodes/w1", &status); code != 404 || status.Message != `nodes.metrics.k8s.io "w1" not found` {
		t.Fatalf("one scrape: %d %+v", code, status)
	}
	s.scrapeAll(ctx)
	var nm metricsv1beta1.NodeMetrics
	if code := get("/apis/metrics.k8s.io/v1beta1/nodes/w1", &nm); code != 200 || nm.Usage[corev1.ResourceCPU].String() != "5000000000n" ||
		nm.Usage[corev1.ResourceMemory].String() != "1400000000" || nm.Window.Duration != 10*time.Second || nm.Labels["pool"] != "a" || nm.Kind != "NodeMetrics" {
		t.Fatalf("node metrics: %d %+v", code, nm)
	}
	var pods metricsv1beta1.PodMetricsList
	if code := get("/apis/metrics.k8s.io/v1beta1/namespaces/ns/pods?labelSelector=app%3Dweb", &pods); code != 200 || len(pods.Items) != 1 ||
		len(pods.Items[0].Containers) != 2 || pods.Items[0].Containers[0].Name != "app" ||
		pods.Items[0].Containers[0].Usage[corev1.ResourceCPU].String() != "500000000n" || pods.Items[0].Containers[0].Usage[corev1.ResourceMemory].String() != "10Mi" {
		t.Fatalf("pod metrics: %d %+v", code, pods)
	}
	if code := get("/apis/metrics.k8s.io/v1beta1/pods?labelSelector=app%3Dother", &pods); code != 200 || len(pods.Items) != 0 {
		t.Fatalf("selector: %+v", pods)
	}
	for path, want := range map[string]string{
		"/apis/metrics.k8s.io/v1beta1/nodes/nope":                   `node "nope" not found`,
		"/apis/metrics.k8s.io/v1beta1/namespaces/ns/pods/nope":      `pods "nope" not found`,
		"/apis/metrics.k8s.io/v1beta1/namespaces/default/pods/nope": `pods "nope" not found`,
	} {
		status = metav1.Status{}
		if code := get(path, &status); code != 404 || status.Message != want {
			t.Errorf("%s: %d %q", path, code, status.Message)
		}
	}
	var list map[string]any
	if code := get("/apis/metrics.k8s.io/v1beta1", &list); code != 200 || list["groupVersion"] != "metrics.k8s.io/v1beta1" {
		t.Fatalf("discovery: %v", list)
	}
	req, _ := http.NewRequest(http.MethodGet, api.URL+"/apis/metrics.k8s.io/v1beta1/nodes", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("requests need the token: %v %v", resp, err)
	}
}
