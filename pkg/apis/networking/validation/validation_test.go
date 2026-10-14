package validation

import (
	"strings"
	"testing"

	networkingv1 "mini-k8s/pkg/apis/networking/v1"
)

func ptr(s string) *string { return &s }

func backend(name string, number int32, port string) networkingv1.IngressBackend {
	return networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: name,
		Port: networkingv1.ServiceBackendPort{Number: number, Name: port}}}
}

func TestValidIngress(t *testing.T) {
	def := backend("web", 80, "")
	ing := &networkingv1.Ingress{Spec: networkingv1.IngressSpec{DefaultBackend: &def, Rules: []networkingv1.IngressRule{
		{Host: "foo.example.com", HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{
			{Path: "/api", PathType: ptr(networkingv1.PathTypePrefix), Backend: backend("api", 0, "http")}}}},
		{Host: "*.example.com", HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{
			{PathType: ptr(networkingv1.PathTypeImplementationSpecific), Backend: backend("web", 80, "")}}}},
	}}}
	if errs := ValidateIngress(ing); len(errs) != 0 {
		t.Fatal(errs)
	}
}

func TestInvalidIngress(t *testing.T) {
	if errs := ValidateIngress(&networkingv1.Ingress{}); len(errs) != 1 ||
		errs[0] != "spec: Invalid value: null: either `defaultBackend` or `rules` must be specified" {
		t.Fatal(errs)
	}
	ing := &networkingv1.Ingress{Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{
		{Host: "Foo_bar", HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{
			{Path: "api", Backend: backend("web", 80, "")},
			{Path: "api", PathType: ptr(networkingv1.PathTypePrefix), Backend: backend("web", 80, "http")},
			{Path: "/", PathType: ptr("Regex"), Backend: backend("web", 0, "")},
		}}},
		{Host: "10.0.0.1"},
		{Host: "a.*.example.com"},
	}}}
	got := strings.Join(ValidateIngress(ing), "\n")
	for _, want := range []string{
		`spec.rules[0].host: Invalid value: "Foo_bar": a lowercase RFC 1123 subdomain must consist of`,
		"spec.rules[0].http.paths[0].pathType: Required value: pathType must be specified",
		`spec.rules[0].http.paths[1].path: Invalid value: "api": must be an absolute path`,
		`spec.rules[0].http.paths[1].backend: Invalid value: "": cannot set both port name & port number`,
		`spec.rules[0].http.paths[2].pathType: Unsupported value: "Regex"`,
		`spec.rules[0].http.paths[2].backend: Invalid value: "": port name or number is required`,
		`spec.rules[1].host: Invalid value: "10.0.0.1": must be a DNS name, not an IP address`,
		`spec.rules[2].host: Invalid value: "a.*.example.com"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "paths[0].path:") {
		t.Errorf("a path without pathType is not checked further:\n%s", got)
	}
}
