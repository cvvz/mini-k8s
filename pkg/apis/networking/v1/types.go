// Package v1 contains the networking.k8s.io/v1 types (networkingv1):
// Ingress, served by the Ingress controller on 127.0.0.1:18080.
package v1

import (
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

// Ingress routes HTTP requests by host and path to Services.
type Ingress struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              IngressSpec   `json:"spec"`
	Status            IngressStatus `json:"status,omitzero"`
}

// IngressSpec lists the routing rules.
type IngressSpec struct {
	// IngressClassName selects the controller; mini's controller serves
	// Ingresses without a class and those of class "mini".
	IngressClassName *string `json:"ingressClassName,omitempty"`
	// DefaultBackend serves requests no rule matches.
	DefaultBackend *IngressBackend `json:"defaultBackend,omitempty"`
	Rules          []IngressRule   `json:"rules,omitempty"`
}

// IngressRule matches a host (empty: every host; "*.example.com": one
// extra label) and routes its paths.
type IngressRule struct {
	Host string                `json:"host,omitempty"`
	HTTP *HTTPIngressRuleValue `json:"http,omitempty"`
}

// HTTPIngressRuleValue lists the paths of a rule.
type HTTPIngressRuleValue struct {
	Paths []HTTPIngressPath `json:"paths"`
}

// Path types.
const (
	PathTypeExact                  = "Exact"
	PathTypePrefix                 = "Prefix"
	PathTypeImplementationSpecific = "ImplementationSpecific"
)

// HTTPIngressPath routes requests whose path matches to a backend.
type HTTPIngressPath struct {
	Path string `json:"path,omitempty"`
	// PathType is Exact, Prefix (matched element-wise: /foo matches /foo/bar
	// but not /foobar) or ImplementationSpecific (Prefix in mini).
	PathType *string        `json:"pathType"`
	Backend  IngressBackend `json:"backend"`
}

// IngressBackend is where matching requests go.
type IngressBackend struct {
	Service *IngressServiceBackend `json:"service,omitempty"`
}

// IngressServiceBackend names a Service port.
type IngressServiceBackend struct {
	Name string             `json:"name"`
	Port ServiceBackendPort `json:"port,omitzero"`
}

// ServiceBackendPort is a Service port number or name; exactly one is set.
type ServiceBackendPort struct {
	Name   string `json:"name,omitempty"`
	Number int32  `json:"number,omitempty"`
}

// IngressStatus reports where the Ingress is served.
type IngressStatus struct {
	LoadBalancer IngressLoadBalancerStatus `json:"loadBalancer"`
}

// IngressLoadBalancerStatus lists the addresses serving the Ingress.
type IngressLoadBalancerStatus struct {
	Ingress []IngressLoadBalancerIngress `json:"ingress,omitempty"`
}

// IngressLoadBalancerIngress is one such address.
type IngressLoadBalancerIngress struct {
	IP       string `json:"ip,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}
