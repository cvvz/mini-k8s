package ingress

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	networkingv1 "mini-k8s/pkg/apis/networking/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
)

// Controller serves HTTP for the Ingresses of its class.
type Controller struct {
	client    *client.Client
	address   networkingv1.IngressLoadBalancerIngress
	ingresses *informers.Informer[networkingv1.Ingress, *networkingv1.Ingress]
	services  *informers.Informer[corev1.Service, *corev1.Service]
	endpoints *informers.Informer[corev1.Endpoints, *corev1.Endpoints]
	trigger   chan struct{}
	proxy     *httputil.ReverseProxy

	mu    sync.RWMutex
	table *Table
	// next counts requests per backend for round-robin.
	next sync.Map // backend key -> *atomic.Uint64
}

type targetKey struct{}

// NewController returns a controller that reports statusHost (such as
// "localhost") as the address of the Ingresses it serves.
func NewController(c *client.Client, statusHost string) *Controller {
	ctrl := &Controller{
		client:    c,
		address:   networkingv1.IngressLoadBalancerIngress{Hostname: statusHost},
		ingresses: informers.New(client.Ingresses(c, ""), client.ListOptions{}, 0),
		services:  informers.New(client.Services(c, ""), client.ListOptions{}, 0),
		endpoints: informers.New(client.Endpoints(c, ""), client.ListOptions{}, 0),
		trigger:   make(chan struct{}, 1),
		table:     &Table{},
	}
	ctrl.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "http", Host: r.In.Context().Value(targetKey{}).(string)})
			r.Out.Host = r.In.Host // backends see the host the client asked for
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "backend unavailable: "+err.Error(), http.StatusBadGateway)
		},
	}
	poke := func() {
		select {
		case ctrl.trigger <- struct{}{}:
		default:
		}
	}
	ctrl.ingresses.AddHandler(informers.Handler[*networkingv1.Ingress]{OnAdd: func(*networkingv1.Ingress) { poke() },
		OnUpdate: func(_, _ *networkingv1.Ingress) { poke() }, OnDelete: func(*networkingv1.Ingress) { poke() }})
	ctrl.services.AddHandler(informers.Handler[*corev1.Service]{OnAdd: func(*corev1.Service) { poke() },
		OnUpdate: func(_, _ *corev1.Service) { poke() }, OnDelete: func(*corev1.Service) { poke() }})
	ctrl.endpoints.AddHandler(informers.Handler[*corev1.Endpoints]{OnAdd: func(*corev1.Endpoints) { poke() },
		OnUpdate: func(_, _ *corev1.Endpoints) { poke() }, OnDelete: func(*corev1.Endpoints) { poke() }})
	return ctrl
}

// Run keeps the routing table and the Ingress statuses current until ctx ends.
func (c *Controller) Run(ctx context.Context) {
	go c.ingresses.Run(ctx)
	go c.services.Run(ctx)
	go c.endpoints.Run(ctx)
	informers.WaitForSync(ctx, c.ingresses.HasSynced, c.services.HasSynced, c.endpoints.HasSynced)
	// Status writes can fail while the API is down: retry them periodically.
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		ingresses := c.ingresses.List("", nil)
		table := BuildTable(ingresses, c.services.List("", nil), c.endpoints.List("", nil))
		c.mu.Lock()
		c.table = table
		c.mu.Unlock()
		c.updateStatuses(ctx, ingresses)
		select {
		case <-ctx.Done():
			return
		case <-c.trigger:
		case <-ticker.C:
		}
	}
}

// updateStatuses publishes the controller's address on the Ingresses it serves.
func (c *Controller) updateStatuses(ctx context.Context, ingresses []*networkingv1.Ingress) {
	want := []networkingv1.IngressLoadBalancerIngress{c.address}
	for _, ing := range ingresses {
		if !Serves(ing) || ing.DeletionTimestamp != nil || reflect.DeepEqual(ing.Status.LoadBalancer.Ingress, want) {
			continue
		}
		updated := *ing
		updated.Status.LoadBalancer.Ingress = want
		if _, err := client.Ingresses(c.client, ing.Namespace).UpdateStatus(ctx, &updated); err != nil && !client.IsNotFound(err) {
			log.Printf("ingress: update status of %s/%s: %v", ing.Namespace, ing.Name, err)
		}
	}
}

// ServeHTTP proxies to the next ready endpoint of the matching backend:
// 404 when nothing matches, 503 without ready endpoints and 502 when the
// endpoint cannot be reached.
func (c *Controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.RLock()
	b := c.table.Route(r.Host, r.URL.Path)
	c.mu.RUnlock()
	if b == nil {
		http.Error(w, "default backend - 404", http.StatusNotFound)
		return
	}
	if len(b.Targets) == 0 {
		http.Error(w, b.Reason, http.StatusServiceUnavailable)
		return
	}
	counter, _ := c.next.LoadOrStore(b.Key, new(atomic.Uint64))
	target := b.Targets[(counter.(*atomic.Uint64).Add(1)-1)%uint64(len(b.Targets))]
	c.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, target)))
}
