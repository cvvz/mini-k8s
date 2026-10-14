// Package validation validates networking API objects.
package validation

import (
	"fmt"
	"net"
	"strings"

	networkingv1 "mini-k8s/pkg/apis/networking/v1"
	"mini-k8s/pkg/util/validation"
)

const subdomainError = "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', " +
	"and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is " +
	`'[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')`

// ValidateIngress validates an Ingress with the messages of Kubernetes.
func ValidateIngress(ing *networkingv1.Ingress) []string {
	spec := &ing.Spec
	var errs []string
	if spec.DefaultBackend == nil && len(spec.Rules) == 0 {
		errs = append(errs, "spec: Invalid value: null: either `defaultBackend` or `rules` must be specified")
	}
	if c := spec.IngressClassName; c != nil && !validation.IsDNS1123Subdomain(*c) {
		errs = append(errs, fmt.Sprintf("spec.ingressClassName: Invalid value: %q: %s", *c, subdomainError))
	}
	if spec.DefaultBackend != nil {
		errs = append(errs, validateBackend("spec.defaultBackend", spec.DefaultBackend)...)
	}
	for i, rule := range spec.Rules {
		path := fmt.Sprintf("spec.rules[%d]", i)
		if rule.Host != "" {
			errs = append(errs, validateHost(path+".host", rule.Host)...)
		}
		if rule.HTTP == nil {
			continue
		}
		if len(rule.HTTP.Paths) == 0 {
			errs = append(errs, path+".http.paths: Required value")
		}
		for j, p := range rule.HTTP.Paths {
			pp := fmt.Sprintf("%s.http.paths[%d]", path, j)
			if p.PathType == nil {
				errs = append(errs, pp+".pathType: Required value: pathType must be specified")
				continue
			}
			switch *p.PathType {
			case networkingv1.PathTypeExact, networkingv1.PathTypePrefix, networkingv1.PathTypeImplementationSpecific:
				if !strings.HasPrefix(p.Path, "/") && (p.Path != "" || *p.PathType != networkingv1.PathTypeImplementationSpecific) {
					errs = append(errs, fmt.Sprintf("%s.path: Invalid value: %q: must be an absolute path", pp, p.Path))
				}
			default:
				errs = append(errs, fmt.Sprintf(`%s.pathType: Unsupported value: %q: supported values: "Exact", "ImplementationSpecific", "Prefix"`, pp, *p.PathType))
			}
			errs = append(errs, validateBackend(pp+".backend", &p.Backend)...)
		}
	}
	return errs
}

// validateHost accepts DNS names and a wildcard for the first label.
func validateHost(path, host string) []string {
	if net.ParseIP(host) != nil {
		return []string{fmt.Sprintf("%s: Invalid value: %q: must be a DNS name, not an IP address", path, host)}
	}
	if name, _ := strings.CutPrefix(host, "*."); !validation.IsDNS1123Subdomain(name) {
		return []string{fmt.Sprintf("%s: Invalid value: %q: %s", path, host, subdomainError)}
	}
	return nil
}

func validateBackend(path string, b *networkingv1.IngressBackend) []string {
	s := b.Service
	if s == nil {
		return []string{path + ".service: Required value"}
	}
	var errs []string
	if !validation.IsDNS1123Label(s.Name) {
		errs = append(errs, fmt.Sprintf("%s.service.name: Invalid value: %q: must be a DNS label", path, s.Name))
	}
	switch {
	case s.Port.Name != "" && s.Port.Number != 0:
		errs = append(errs, path+`: Invalid value: "": cannot set both port name & port number`)
	case s.Port.Name == "" && s.Port.Number == 0:
		errs = append(errs, path+`: Invalid value: "": port name or number is required`)
	case s.Port.Number != 0 && (s.Port.Number < 1 || s.Port.Number > 65535):
		errs = append(errs, fmt.Sprintf("%s.service.port.number: Invalid value: %d: must be between 1 and 65535, inclusive", path, s.Port.Number))
	case s.Port.Name != "" && (!validation.IsDNS1123Label(s.Port.Name) || len(s.Port.Name) > 15):
		errs = append(errs, fmt.Sprintf("%s.service.port.name: Invalid value: %q: must be a port name of at most 15 lowercase letters, digits and '-'", path, s.Port.Name))
	}
	return errs
}
