// Package ingress is mini's Ingress controller: an HTTP reverse proxy that
// routes requests by host and path to the ready endpoints of Services, like
// ingress-nginx. It runs as a static Pod on the control-plane node
// (hostNetwork, published on 127.0.0.1:18080) and reports where it serves in
// the status of the Ingresses it handles.
package ingress

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	corev1 "mini-k8s/pkg/apis/core/v1"
	networkingv1 "mini-k8s/pkg/apis/networking/v1"
)

// ClassName is the ingressClassName this controller serves besides none.
const ClassName = "mini"

// Serves reports whether the controller handles ing.
func Serves(ing *networkingv1.Ingress) bool {
	return ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName == ClassName
}

// Backend is a Service port with its ready endpoints.
type Backend struct {
	// Key identifies the backend, e.g. "default/web:80".
	Key string
	// Targets are the ready endpoints as host:port, sorted.
	Targets []string
	// Reason explains an empty Targets.
	Reason string
}

type prefixRoute struct {
	prefix  string // without a trailing slash; "" matches every path
	backend *Backend
}

// routes are the paths of one host.
type routes struct {
	exact    map[string]*Backend
	prefixes []prefixRoute // longest first
}

func (r *routes) match(path string) *Backend {
	if b, ok := r.exact[path]; ok {
		return b
	}
	for _, p := range r.prefixes {
		if p.prefix == "" || path == p.prefix || strings.HasPrefix(path, p.prefix+"/") {
			return p.backend
		}
	}
	return nil
}

// Table is a snapshot of the routing rules of every served Ingress.
type Table struct {
	hosts     map[string]*routes // exact host names
	wildcards map[string]*routes // "*.example.com" keyed by "example.com"
	anyHost   *routes            // rules without a host
	fallback  *Backend           // the oldest defaultBackend
}

// Route returns the backend for a request, or nil (404) when no rule and
// no default backend match. A host-specific rule set without a matching
// path falls back to the default backend, as in ingress-nginx.
func (t *Table) Route(host, path string) *Backend {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	r := t.hosts[host]
	if r == nil {
		if _, parent, ok := strings.Cut(host, "."); ok {
			r = t.wildcards[parent]
		}
	}
	if r == nil {
		r = t.anyHost
	}
	if r != nil {
		if b := r.match(path); b != nil {
			return b
		}
	}
	return t.fallback
}

// BuildTable computes the routing table. Older Ingresses win conflicting
// host and path pairs, like ingress-nginx.
func BuildTable(ingresses []*networkingv1.Ingress, services []*corev1.Service, endpoints []*corev1.Endpoints) *Table {
	svcs := map[string]*corev1.Service{}
	for _, s := range services {
		svcs[s.Namespace+"/"+s.Name] = s
	}
	eps := map[string]*corev1.Endpoints{}
	for _, e := range endpoints {
		eps[e.Namespace+"/"+e.Name] = e
	}
	served := make([]*networkingv1.Ingress, 0, len(ingresses))
	for _, ing := range ingresses {
		if Serves(ing) && ing.DeletionTimestamp == nil {
			served = append(served, ing)
		}
	}
	sort.Slice(served, func(i, j int) bool {
		a, b := served[i], served[j]
		if !a.CreationTimestamp.Time.Equal(b.CreationTimestamp.Time) {
			return a.CreationTimestamp.Before(b.CreationTimestamp.Time)
		}
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	t := &Table{hosts: map[string]*routes{}, wildcards: map[string]*routes{}}
	backends := map[string]*Backend{}
	resolve := func(ns string, b *networkingv1.IngressServiceBackend) *Backend {
		port := b.Port.Name
		if port == "" {
			port = strconv.Itoa(int(b.Port.Number))
		}
		key := ns + "/" + b.Name + ":" + port
		if _, ok := backends[key]; !ok {
			backends[key] = resolveBackend(key, svcs[ns+"/"+b.Name], eps[ns+"/"+b.Name], b.Port)
		}
		return backends[key]
	}
	for _, ing := range served {
		if d := ing.Spec.DefaultBackend; d != nil && d.Service != nil && t.fallback == nil {
			t.fallback = resolve(ing.Namespace, d.Service)
		}
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			r := t.routesFor(rule.Host)
			for _, p := range rule.HTTP.Paths {
				if p.Backend.Service == nil || p.PathType == nil {
					continue
				}
				b := resolve(ing.Namespace, p.Backend.Service)
				if *p.PathType == networkingv1.PathTypeExact {
					if _, taken := r.exact[p.Path]; !taken {
						r.exact[p.Path] = b
					}
					continue
				}
				prefix := strings.TrimRight(p.Path, "/")
				if !hasPrefix(r.prefixes, prefix) {
					r.prefixes = append(r.prefixes, prefixRoute{prefix: prefix, backend: b})
				}
			}
		}
	}
	for _, r := range t.all() {
		sort.SliceStable(r.prefixes, func(i, j int) bool { return len(r.prefixes[i].prefix) > len(r.prefixes[j].prefix) })
	}
	return t
}

func hasPrefix(list []prefixRoute, prefix string) bool {
	for _, p := range list {
		if p.prefix == prefix {
			return true
		}
	}
	return false
}

func (t *Table) routesFor(host string) *routes {
	m, key := t.hosts, strings.ToLower(host)
	switch {
	case host == "":
		if t.anyHost == nil {
			t.anyHost = &routes{exact: map[string]*Backend{}}
		}
		return t.anyHost
	case strings.HasPrefix(host, "*."):
		m, key = t.wildcards, strings.ToLower(host[2:])
	}
	if m[key] == nil {
		m[key] = &routes{exact: map[string]*Backend{}}
	}
	return m[key]
}

func (t *Table) all() []*routes {
	var out []*routes
	for _, r := range t.hosts {
		out = append(out, r)
	}
	for _, r := range t.wildcards {
		out = append(out, r)
	}
	if t.anyHost != nil {
		out = append(out, t.anyHost)
	}
	return out
}

// resolveBackend finds the Service port a backend names and the ready
// endpoints behind it.
func resolveBackend(key string, svc *corev1.Service, ep *corev1.Endpoints, port networkingv1.ServiceBackendPort) *Backend {
	b := &Backend{Key: key}
	if svc == nil {
		b.Reason = fmt.Sprintf("service %q not found", key[:strings.LastIndexByte(key, ':')])
		return b
	}
	var sp *corev1.ServicePort
	for i := range svc.Spec.Ports {
		p := &svc.Spec.Ports[i]
		if p.Protocol == corev1.ProtocolTCP && ((port.Name != "" && p.Name == port.Name) || (port.Name == "" && p.Port == port.Number)) {
			sp = p
		}
	}
	if sp == nil {
		b.Reason = fmt.Sprintf("service %q has no port %s", svc.Namespace+"/"+svc.Name, key[strings.LastIndexByte(key, ':')+1:])
		return b
	}
	if ep != nil {
		for _, ss := range ep.Subsets {
			for _, p := range ss.Ports {
				if p.Name != sp.Name || p.Protocol != corev1.ProtocolTCP {
					continue
				}
				for _, a := range ss.Addresses {
					b.Targets = append(b.Targets, net.JoinHostPort(a.IP, strconv.Itoa(int(p.Port))))
				}
			}
		}
	}
	sort.Strings(b.Targets)
	if len(b.Targets) == 0 {
		b.Reason = fmt.Sprintf("no endpoints available for service %q", svc.Namespace+"/"+svc.Name)
	}
	return b
}
