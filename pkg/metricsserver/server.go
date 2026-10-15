package metricsserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mini-k8s/pkg/api/resource"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	metricsv1beta1 "mini-k8s/pkg/apis/metrics/v1beta1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/labels"
)

// Server scrapes kubelets and serves the metrics API.
type Server struct {
	nodes      *informers.Informer[corev1.Node, *corev1.Node]
	pods       *informers.Informer[corev1.Pod, *corev1.Pod]
	storage    *storage
	scraper    *scraper
	token      string
	resolution time.Duration
	scraped    atomic.Bool
	now        func() time.Time
}

// New returns a metrics server that scrapes every resolution, using token
// both with kubelets and to authenticate its own clients.
func New(c *client.Client, token string, kubeletPort int, resolution time.Duration) *Server {
	return &Server{
		nodes:   informers.New(client.Nodes(c), client.ListOptions{}, 0),
		pods:    informers.New(client.Pods(c, ""), client.ListOptions{}, 0),
		storage: newStorage(), token: token, resolution: resolution, now: time.Now,
		scraper: &scraper{http: &http.Client{Timeout: 10 * time.Second}, token: token, kubeletPort: kubeletPort},
	}
}

// Run scrapes until ctx ends.
func (s *Server) Run(ctx context.Context) {
	go s.nodes.Run(ctx)
	go s.pods.Run(ctx)
	informers.WaitForSync(ctx, s.nodes.HasSynced, s.pods.HasSynced)
	ticker := time.NewTicker(s.resolution)
	defer ticker.Stop()
	for {
		s.scrapeAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// scrapeAll scrapes every node with an address at once.
func (s *Server) scrapeAll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, max(s.resolution, 5*time.Second))
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	all := newBatch()
	for _, n := range s.nodes.List("", nil) {
		address := n.InternalIP()
		if address == "" {
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			b := newBatch()
			if err := s.scraper.scrape(ctx, name, address, b); err != nil {
				log.Printf("metrics-server: scrape %s: %v", name, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for k, v := range b.nodes {
				all.nodes[k] = v
			}
			for k, v := range b.pods {
				all.pods[k] = v
			}
		}(n.Name)
	}
	wg.Wait()
	s.storage.store(all)
	s.scraped.Store(true)
}

// Handler serves the metrics.k8s.io/v1beta1 API and health checks.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/livez":
			_, _ = w.Write([]byte("ok"))
			return
		case "/readyz":
			if !s.scraped.Load() {
				http.Error(w, "no scrape yet", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("ok"))
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) != 1 {
			writeStatus(w, http.StatusUnauthorized, metav1.ReasonUnauthorized, "Unauthorized")
			return
		}
		if r.Method != http.MethodGet {
			writeStatus(w, http.StatusMethodNotAllowed, metav1.ReasonMethodNotAllowed, fmt.Sprintf("%s is not supported", r.Method))
			return
		}
		sel, err := labels.Parse(r.URL.Query().Get("labelSelector"))
		if err != nil {
			writeStatus(w, http.StatusBadRequest, metav1.ReasonBadRequest, err.Error())
			return
		}
		rest, ok := strings.CutPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/apis/"+metricsv1beta1.GroupVersion)
		if !ok {
			writeStatus(w, http.StatusNotFound, metav1.ReasonNotFound, "the server could not find the requested resource")
			return
		}
		segs := strings.Split(strings.TrimPrefix(rest, "/"), "/")
		switch {
		case rest == "":
			writeJSON(w, http.StatusOK, resourceList())
		case segs[0] == "nodes" && len(segs) == 1:
			writeJSON(w, http.StatusOK, s.nodeList(sel))
		case segs[0] == "nodes" && len(segs) == 2:
			s.getNode(w, segs[1])
		case segs[0] == "pods" && len(segs) == 1:
			writeJSON(w, http.StatusOK, s.podList("", sel))
		case segs[0] == "namespaces" && len(segs) == 3 && segs[2] == "pods":
			writeJSON(w, http.StatusOK, s.podList(segs[1], sel))
		case segs[0] == "namespaces" && len(segs) == 4 && segs[2] == "pods":
			s.getPod(w, segs[1], segs[3])
		default:
			writeStatus(w, http.StatusNotFound, metav1.ReasonNotFound, "the server could not find the requested resource")
		}
	})
}

func resourceList() map[string]any {
	verbs := []string{"get", "list"}
	return map[string]any{"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": metricsv1beta1.GroupVersion,
		"resources": []any{
			map[string]any{"name": "nodes", "singularName": "", "namespaced": false, "kind": "NodeMetrics", "verbs": verbs},
			map[string]any{"name": "pods", "singularName": "", "namespaced": true, "kind": "PodMetrics", "verbs": verbs},
		}}
}

func (s *Server) nodeMetrics(n *corev1.Node) (metricsv1beta1.NodeMetrics, bool) {
	u, ok := s.storage.node(n.Name)
	if !ok {
		return metricsv1beta1.NodeMetrics{}, false
	}
	return metricsv1beta1.NodeMetrics{
		TypeMeta:   metav1.TypeMeta{Kind: "NodeMetrics", APIVersion: metricsv1beta1.GroupVersion},
		ObjectMeta: metav1.ObjectMeta{Name: n.Name, Labels: n.Labels, CreationTimestamp: metav1.Time{Time: s.now()}},
		Timestamp:  metav1.Time{Time: u.at}, Window: metav1.Duration{Duration: u.window},
		Usage: corev1.ResourceList{corev1.ResourceCPU: resource.NewNanoQuantity(u.nanoCores),
			corev1.ResourceMemory: resource.NewBinaryQuantity(u.memory)},
	}, true
}

func (s *Server) nodeList(sel labels.Selector) metricsv1beta1.NodeMetricsList {
	list := metricsv1beta1.NodeMetricsList{TypeMeta: metav1.TypeMeta{Kind: "NodeMetricsList", APIVersion: metricsv1beta1.GroupVersion},
		Items: []metricsv1beta1.NodeMetrics{}}
	for _, n := range s.nodes.List("", sel) {
		if m, ok := s.nodeMetrics(n); ok {
			list.Items = append(list.Items, m)
		}
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list
}

func (s *Server) getNode(w http.ResponseWriter, name string) {
	n, ok := s.nodes.Get("", name)
	if !ok {
		writeStatus(w, http.StatusNotFound, metav1.ReasonNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	m, ok := s.nodeMetrics(n)
	if !ok {
		writeStatus(w, http.StatusNotFound, metav1.ReasonNotFound, fmt.Sprintf("nodes.metrics.k8s.io %q not found", name))
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) podMetrics(p *corev1.Pod) (metricsv1beta1.PodMetrics, bool) {
	containers, ok := s.storage.pod(p.Namespace, p.Name)
	if !ok {
		return metricsv1beta1.PodMetrics{}, false
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].name < containers[j].name })
	m := metricsv1beta1.PodMetrics{
		TypeMeta:   metav1.TypeMeta{Kind: "PodMetrics", APIVersion: metricsv1beta1.GroupVersion},
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: p.Labels, CreationTimestamp: metav1.Time{Time: s.now()}},
	}
	for _, c := range containers {
		// The Pod's sample time and window are its oldest container's.
		if m.Timestamp.IsZero() || c.at.Before(m.Timestamp.Time) {
			m.Timestamp = metav1.Time{Time: c.at}
		}
		m.Window.Duration = max(m.Window.Duration, c.window)
		m.Containers = append(m.Containers, metricsv1beta1.ContainerMetrics{Name: c.name, Usage: corev1.ResourceList{
			corev1.ResourceCPU: resource.NewNanoQuantity(c.nanoCores), corev1.ResourceMemory: resource.NewBinaryQuantity(c.memory)}})
	}
	return m, true
}

func (s *Server) podList(namespace string, sel labels.Selector) metricsv1beta1.PodMetricsList {
	list := metricsv1beta1.PodMetricsList{TypeMeta: metav1.TypeMeta{Kind: "PodMetricsList", APIVersion: metricsv1beta1.GroupVersion},
		Items: []metricsv1beta1.PodMetrics{}}
	for _, p := range s.pods.List(namespace, sel) {
		if m, ok := s.podMetrics(p); ok {
			list.Items = append(list.Items, m)
		}
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].Namespace+"/"+list.Items[i].Name < list.Items[j].Namespace+"/"+list.Items[j].Name
	})
	return list
}

func (s *Server) getPod(w http.ResponseWriter, namespace, name string) {
	p, ok := s.pods.Get(namespace, name)
	if !ok {
		writeStatus(w, http.StatusNotFound, metav1.ReasonNotFound, fmt.Sprintf("pods %q not found", name))
		return
	}
	m, ok := s.podMetrics(p)
	if !ok {
		writeStatus(w, http.StatusNotFound, metav1.ReasonNotFound, fmt.Sprintf("pods.metrics.k8s.io %q not found", name))
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	writeJSON(w, code, metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: "Failure",
		Message: message, Reason: reason, Code: int32(code)})
}
