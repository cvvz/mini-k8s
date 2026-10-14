package ingress

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	networkingv1 "mini-k8s/pkg/apis/networking/v1"
	"mini-k8s/pkg/client"
)

func ptr(s string) *string { return &s }

func svcBackend(name string, number int32, port string) networkingv1.IngressBackend {
	return networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: name,
		Port: networkingv1.ServiceBackendPort{Number: number, Name: port}}}
}

func path(p, typ string, b networkingv1.IngressBackend) networkingv1.HTTPIngressPath {
	return networkingv1.HTTPIngressPath{Path: p, PathType: ptr(typ), Backend: b}
}

func rule(host string, paths ...networkingv1.HTTPIngressPath) networkingv1.IngressRule {
	return networkingv1.IngressRule{Host: host, HTTP: &networkingv1.HTTPIngressRuleValue{Paths: paths}}
}

func ingress(name string, created int, rules ...networkingv1.IngressRule) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", CreationTimestamp: metav1.NewTime(time.Unix(int64(created), 0))},
		Spec:       networkingv1.IngressSpec{Rules: rules},
	}
}

// service returns a Service with an "http" port 80 and its Endpoints with
// the given ready addresses on targetPort.
func service(name string, targetPort int32, ips ...string) (*corev1.Service, *corev1.Endpoints) {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP}}}}
	ep := &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
	if len(ips) > 0 {
		ss := corev1.EndpointSubset{Ports: []corev1.EndpointPort{{Name: "http", Port: targetPort, Protocol: corev1.ProtocolTCP}}}
		for _, ip := range ips {
			ss.Addresses = append(ss.Addresses, corev1.EndpointAddress{IP: ip})
		}
		ep.Subsets = []corev1.EndpointSubset{ss}
	}
	return svc, ep
}

func TestRouting(t *testing.T) {
	var services []*corev1.Service
	var endpoints []*corev1.Endpoints
	for i, name := range []string{"api", "web", "docs", "exact", "wild", "other", "old"} {
		s, e := service(name, 8080, fmt.Sprintf("10.244.1.%d", i+1))
		services, endpoints = append(services, s), append(endpoints, e)
	}
	empty, _ := service("empty", 8080)
	services = append(services, empty)
	def := svcBackend("web", 80, "")
	main := ingress("main", 2,
		rule("foo.example.com",
			path("/api", networkingv1.PathTypePrefix, svcBackend("api", 0, "http")),
			path("/api/docs/", networkingv1.PathTypePrefix, svcBackend("docs", 80, "")),
			path("/exact", networkingv1.PathTypeExact, svcBackend("exact", 80, ""))),
		rule("*.example.com", path("/", networkingv1.PathTypePrefix, svcBackend("wild", 80, ""))),
		rule("", path("/down", networkingv1.PathTypePrefix, svcBackend("empty", 80, "")),
			path("/missing", networkingv1.PathTypePrefix, svcBackend("nope", 80, ""))))
	main.Spec.DefaultBackend = &def
	older := ingress("older", 1, rule("foo.example.com", path("/api", networkingv1.PathTypePrefix, svcBackend("old", 80, ""))))
	foreign := ingress("foreign", 0, rule("", path("/", networkingv1.PathTypePrefix, svcBackend("other", 80, ""))))
	foreign.Spec.IngressClassName = ptr("nginx")
	table := BuildTable([]*networkingv1.Ingress{main, older, foreign}, services, endpoints)

	for _, tc := range []struct{ host, path, want string }{
		{"foo.example.com", "/api", "default/old:80"}, // the older Ingress wins the same host and path
		{"foo.example.com:18080", "/api/v1", "default/old:80"},
		{"FOO.example.com", "/api/docs/x", "default/docs:80"}, // longest prefix
		{"foo.example.com", "/apis", "default/web:80"},        // element-wise: falls to the default backend
		{"foo.example.com", "/exact", "default/exact:80"},
		{"foo.example.com", "/exact/x", "default/web:80"},
		{"bar.example.com", "/anything", "default/wild:80"},
		{"a.bar.example.com", "/down", "default/empty:80"}, // wildcards match one label only
		{"example.com", "/missing", "default/nope:80"},
	} {
		b := table.Route(tc.host, tc.path)
		if b == nil || b.Key != tc.want {
			t.Errorf("Route(%s, %s) = %+v, want %s", tc.host, tc.path, b, tc.want)
		}
	}
	if b := table.Route("foo.example.com", "/api"); len(b.Targets) != 1 || b.Targets[0] != "10.244.1.7:8080" {
		t.Errorf("targets: %v", b.Targets)
	}
	if b := table.Route("x", "/down"); b.Reason != `no endpoints available for service "default/empty"` {
		t.Errorf("reason: %q", b.Reason)
	}
	if b := table.Route("x", "/missing"); b.Reason != `service "default/nope" not found` {
		t.Errorf("reason: %q", b.Reason)
	}
	if b := BuildTable([]*networkingv1.Ingress{foreign}, services, endpoints).Route("x", "/"); b != nil {
		t.Errorf("Ingresses of another class are ignored, got %+v", b)
	}
}

func TestServeHTTP(t *testing.T) {
	var targets []string
	for i := range 2 {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%d %s %s %s", i, r.Host, r.URL.RequestURI(), r.Header.Get("X-Forwarded-Host"))
		}))
		t.Cleanup(backend.Close)
		targets = append(targets, backend.Listener.Addr().String())
	}
	ctrl := NewController(client.New("http://127.0.0.1:1", ""), "localhost")
	ctrl.table = &Table{hosts: map[string]*routes{}, wildcards: map[string]*routes{}, anyHost: &routes{exact: map[string]*Backend{},
		prefixes: []prefixRoute{{prefix: "/app", backend: &Backend{Key: "default/web:80", Targets: targets}},
			{prefix: "/down", backend: &Backend{Key: "default/down:80", Reason: "no endpoints available for service \"default/down\""}}}}}
	frontend := httptest.NewServer(ctrl)
	t.Cleanup(frontend.Close)
	get := func(path string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, frontend.URL+path, nil)
		req.Host = "shop.example.com"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	seen := map[string]bool{}
	for range 4 {
		code, body := get("/app/x?y=1")
		if code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		seen[body[:1]] = true
		if want := " shop.example.com /app/x?y=1 shop.example.com"; body[1:] != want {
			t.Fatalf("got %q, want host, path, query and X-Forwarded-Host preserved", body)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("round-robin reached %v", seen)
	}
	if code, body := get("/down"); code != http.StatusServiceUnavailable || body != "no endpoints available for service \"default/down\"\n" {
		t.Fatalf("%d %q", code, body)
	}
	if code, body := get("/nothing"); code != http.StatusNotFound || body != "default backend - 404\n" {
		t.Fatalf("%d %q", code, body)
	}
}
