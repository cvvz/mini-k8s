// Package proxy contains the HTTP gateway that exposes the cluster's Service
// on 127.0.0.1:18080.
package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/labels"
)

// Gateway load-balances HTTP requests round-robin across the ready Pods
// selected by the cluster's Service.
type Gateway struct {
	services *informers.Informer[corev1.Service, *corev1.Service]
	pods     *informers.Informer[corev1.Pod, *corev1.Pod]
	nodes    *informers.Informer[corev1.Node, *corev1.Node]
	trigger  chan struct{}

	mu      sync.RWMutex
	targets []*url.URL
	reason  string
	next    atomic.Uint64
}

// NewGateway creates a gateway backed by its own informers.
func NewGateway(c *client.Client) *Gateway {
	g := &Gateway{
		services: informers.New(client.Services(c, ""), client.ListOptions{}, 0),
		pods:     informers.New(client.Pods(c, ""), client.ListOptions{}, 0),
		nodes:    informers.New(client.Nodes(c), client.ListOptions{}, 0),
		trigger:  make(chan struct{}, 1),
		reason:   "gateway is starting",
	}
	poke := func() {
		select {
		case g.trigger <- struct{}{}:
		default:
		}
	}
	g.services.AddHandler(informers.Handler[*corev1.Service]{OnAdd: func(*corev1.Service) { poke() }, OnUpdate: func(_, _ *corev1.Service) { poke() }, OnDelete: func(*corev1.Service) { poke() }})
	g.pods.AddHandler(informers.Handler[*corev1.Pod]{OnAdd: func(*corev1.Pod) { poke() }, OnUpdate: func(_, _ *corev1.Pod) { poke() }, OnDelete: func(*corev1.Pod) { poke() }})
	g.nodes.AddHandler(informers.Handler[*corev1.Node]{OnAdd: func(*corev1.Node) { poke() }, OnUpdate: func(_, _ *corev1.Node) { poke() }, OnDelete: func(*corev1.Node) { poke() }})
	return g
}

// Run keeps the backend list current until ctx ends.
func (g *Gateway) Run(ctx context.Context) {
	go g.services.Run(ctx)
	go g.pods.Run(ctx)
	go g.nodes.Run(ctx)
	informers.WaitForSync(ctx, g.services.HasSynced, g.pods.HasSynced, g.nodes.HasSynced)
	// Node readiness ages out without an event, so also refresh periodically.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		g.refresh()
		select {
		case <-ctx.Done():
			return
		case <-g.trigger:
		case <-ticker.C:
		}
	}
}

func (g *Gateway) refresh() {
	targets, reason := Backends(g.services.List("", nil), g.pods.List("", nil), g.nodes.List("", nil), time.Now())
	g.mu.Lock()
	g.targets, g.reason = targets, reason
	g.mu.Unlock()
}

// Backends computes the gateway targets: ready Pods on ready nodes that
// match the (single) Service selector, sorted for stable round-robin.
func Backends(services []*corev1.Service, pods []*corev1.Pod, nodes []*corev1.Node, now time.Time) ([]*url.URL, string) {
	if len(services) == 0 {
		return nil, "no Service exists; create one to expose Pods on this gateway"
	}
	svc := services[0]
	readyNodes := map[string]bool{}
	for _, n := range nodes {
		readyNodes[n.Name] = corev1.IsNodeReady(n, now)
	}
	sel := labels.SelectorFromSet(svc.Spec.Selector)
	var targets []*url.URL
	for _, p := range pods {
		if p.Namespace != svc.Namespace || !sel.Matches(p.Labels) || p.DeletionTimestamp != nil ||
			!corev1.IsPodReady(p) || p.Status.PodIP == "" || !readyNodes[p.Spec.NodeName] {
			continue
		}
		targets = append(targets, &url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", p.Status.PodIP, svc.Spec.Ports[0].TargetPort)})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Host < targets[j].Host })
	if len(targets) == 0 {
		return nil, fmt.Sprintf("Service %s/%s has no ready endpoints", svc.Namespace, svc.Name)
	}
	return targets, ""
}

// ServeHTTP proxies to the next backend; 503 without backends, 502 when a
// backend cannot be reached.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.RLock()
	targets, reason := g.targets, g.reason
	g.mu.RUnlock()
	if len(targets) == 0 {
		http.Error(w, reason, http.StatusServiceUnavailable)
		return
	}
	target := targets[(g.next.Add(1)-1)%uint64(len(targets))]
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, "backend unavailable: "+err.Error(), http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}
