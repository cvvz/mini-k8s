package apiserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/scheme"
)

// aggregated is an API group served by another component, like an
// APIService of kube-aggregator: requests are proxied to a ready endpoint
// of its Service. mini has one, fixed: metrics.k8s.io by metrics-server.
type aggregated struct {
	group, version     string
	namespace, service string
}

// proxyTimeout bounds a proxied request.
const proxyTimeout = 30 * time.Second

var aggregatedAPIs = []aggregated{{group: "metrics.k8s.io", version: "v1beta1", namespace: "kube-system", service: "metrics-server"}}

// aggregatedFor returns the aggregated API a path belongs to.
func aggregatedFor(path string) (aggregated, bool) {
	for _, a := range aggregatedAPIs {
		if path == "/apis/"+a.group || strings.HasPrefix(path, "/apis/"+a.group+"/") {
			return a, true
		}
	}
	return aggregated{}, false
}

func (a aggregated) groupVersion() string { return a.group + "/" + a.version }

// unavailable is the aggregator's answer when the backing Service has no
// ready endpoint or does not answer.
func unavailable() error {
	return newStatus(http.StatusServiceUnavailable, metav1.ReasonServiceUnavail, "the server is currently unable to handle the request")
}

// proxyAggregated serves the group document itself and forwards the rest,
// authenticated with the API server's own token.
func (s *Server) proxyAggregated(w http.ResponseWriter, r *http.Request, a aggregated) {
	if strings.TrimSuffix(r.URL.Path, "/") == "/apis/"+a.group {
		gv := map[string]string{"groupVersion": a.groupVersion(), "version": a.version}
		writeJSON(w, http.StatusOK, map[string]any{"kind": "APIGroup", "apiVersion": "v1", "name": a.group,
			"versions": []any{gv}, "preferredVersion": gv})
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, newStatus(http.StatusMethodNotAllowed, metav1.ReasonMethodNotAllowed, "%s is not supported on this path", r.Method))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), proxyTimeout)
	defer cancel()
	host, err := s.serviceEndpoint(ctx, a.namespace, a.service)
	if err != nil {
		writeError(w, err)
		return
	}
	u := url.URL{Scheme: "http", Host: host, Path: r.URL.Path, RawQuery: r.URL.RawQuery}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	resp, err := s.kubelet.Do(req)
	if err != nil {
		writeError(w, unavailable())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// serviceEndpoint returns host:port of a ready endpoint of a Service.
func (s *Server) serviceEndpoint(ctx context.Context, namespace, name string) (string, error) {
	obj, err := s.get(ctx, scheme.Endpoints, namespace, name)
	if err != nil {
		return "", unavailable()
	}
	for _, sub := range obj.(*corev1.Endpoints).Subsets {
		if len(sub.Addresses) > 0 && len(sub.Ports) > 0 {
			return net.JoinHostPort(sub.Addresses[0].IP, strconv.Itoa(int(sub.Ports[0].Port))), nil
		}
	}
	return "", unavailable()
}

// proxyNode forwards GET /api/v1/nodes/{name}/proxy/{path} to the node's
// kubelet, as the nodes/proxy subresource does (for its /metrics/resource,
// say).
func (s *Server) proxyNode(w http.ResponseWriter, r *http.Request, name, path string) {
	if r.Method != http.MethodGet {
		writeError(w, newStatus(http.StatusMethodNotAllowed, metav1.ReasonMethodNotAllowed, "%s is not supported on this path", r.Method))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), proxyTimeout)
	defer cancel()
	obj, err := s.get(ctx, scheme.Nodes, "", name)
	if err != nil {
		writeError(w, err)
		return
	}
	ip := obj.(*corev1.Node).InternalIP()
	if ip == "" {
		writeError(w, newStatus(http.StatusServiceUnavailable, metav1.ReasonServiceUnavail, "node %s reports no InternalIP", name))
		return
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(ip, strconv.Itoa(s.cfg.KubeletPort)), Path: path, RawQuery: r.URL.RawQuery}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	resp, err := s.kubelet.Do(req)
	if err != nil {
		writeError(w, newStatus(http.StatusServiceUnavailable, metav1.ReasonServiceUnavail, "kubelet on %s: %v", name, err))
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
