package mini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

type Proxy struct {
	mu      sync.RWMutex
	targets []string
	err     error
	next    atomic.Uint64
}

func NewProxy() *Proxy { return &Proxy{} }
func (p *Proxy) setTargets(targets []string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.targets = targets
	p.err = err
}
func (p *Proxy) Refresh(ctx context.Context, c *Client) error {
	services, err := c.List(ctx, "Service")
	if err != nil {
		p.setTargets(nil, err)
		return err
	}
	if len(services) == 0 {
		p.setTargets(nil, nil)
		return nil
	}
	var spec ServiceSpec
	if err := json.Unmarshal(services[0].Spec, &spec); err != nil {
		p.setTargets(nil, err)
		return err
	}
	pods, err := c.List(ctx, "Pod")
	if err != nil {
		p.setTargets(nil, err)
		return err
	}
	nodes, err := c.List(ctx, "Node")
	if err != nil {
		p.setTargets(nil, err)
		return err
	}
	live := map[string]bool{}
	for _, n := range nodes {
		if ChooseNode([]Resource{n}, nil, time.Now()) != "" {
			live[n.Metadata.Name] = true
		}
	}
	var targets []string
	for _, pod := range pods {
		if pod.Status.Phase == "Running" && pod.Status.PodIP != "" && live[pod.Status.NodeName] && Matches(spec.Selector, pod.Metadata.Labels) {
			targets = append(targets, fmt.Sprintf("http://%s:%d", pod.Status.PodIP, spec.Ports[0].TargetPort))
		}
	}
	p.setTargets(targets, nil)
	return nil
}
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.RLock()
	if len(p.targets) == 0 {
		err := p.err
		p.mu.RUnlock()
		if err == nil {
			err = errors.New("no running backends; check kk get pods and Service selector")
		}
		http.Error(w, err.Error(), 503)
		return
	}
	target := p.targets[(p.next.Add(1)-1)%uint64(len(p.targets))]
	p.mu.RUnlock()
	u, err := url.Parse(target)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "backend unavailable: "+err.Error(), 502)
	}
	proxy.ServeHTTP(w, r)
}
