// Package validation validates core API objects. Errors use the
// "field.path: Reason: detail" form returned to clients as Status causes.
package validation

import (
	"fmt"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/labels"
	"mini-k8s/pkg/util/validation"
)

// ValidatePodSpec validates a Pod spec found at path.
func ValidatePodSpec(path string, spec *corev1.PodSpec) []string {
	var errs []string
	if len(spec.Containers) != 1 {
		errs = append(errs, path+".containers: Invalid value: exactly one container is supported")
	}
	for i, c := range spec.Containers {
		p := fmt.Sprintf("%s.containers[%d]", path, i)
		if !validation.IsDNS1123Label(c.Name) {
			errs = append(errs, p+".name: Invalid value: must be a DNS label")
		}
		if c.Image == "" {
			errs = append(errs, p+".image: Required value")
		}
		for _, e := range c.Env {
			if !validation.IsEnvVarName(e.Name) {
				errs = append(errs, fmt.Sprintf("%s.env: Invalid value: %q is not a valid variable name", p, e.Name))
			}
		}
		for _, port := range c.Ports {
			if port.ContainerPort < 1 || port.ContainerPort > 65535 {
				errs = append(errs, p+".ports: Invalid value: containerPort must be 1-65535")
			}
			if port.Protocol != "TCP" {
				errs = append(errs, p+".ports: Unsupported value: only TCP is supported")
			}
		}
	}
	if g := spec.TerminationGracePeriodSeconds; g != nil && *g < 0 {
		errs = append(errs, path+".terminationGracePeriodSeconds: Invalid value: must be >= 0")
	}
	if spec.NodeName != "" && !validation.IsDNS1123Subdomain(spec.NodeName) {
		errs = append(errs, path+".nodeName: Invalid value: must be a DNS subdomain")
	}
	return errs
}

// ValidateServiceSpec validates a Service spec.
func ValidateServiceSpec(spec *corev1.ServiceSpec) []string {
	var errs []string
	if len(spec.Selector) == 0 {
		errs = append(errs, "spec.selector: Required value")
	}
	if err := labels.ValidateLabels(spec.Selector); err != nil {
		errs = append(errs, "spec.selector: Invalid value: "+err.Error())
	}
	// Until ClusterIP networking lands, a Service is the single HTTP gateway
	// published on 127.0.0.1:18080.
	if len(spec.Ports) != 1 || spec.Ports[0].Port != 80 {
		errs = append(errs, "spec.ports: Invalid value: exactly one port 80 is supported (the gateway is 127.0.0.1:18080)")
	}
	for _, p := range spec.Ports {
		if p.TargetPort < 1 || p.TargetPort > 65535 {
			errs = append(errs, "spec.ports.targetPort: Invalid value: must be 1-65535")
		}
		if p.Protocol != "TCP" {
			errs = append(errs, "spec.ports.protocol: Unsupported value: only TCP is supported")
		}
	}
	return errs
}

// ValidateEvent validates an Event.
func ValidateEvent(e *corev1.Event) []string {
	var errs []string
	if e.InvolvedObject.Name == "" || e.InvolvedObject.Kind == "" {
		errs = append(errs, "involvedObject: Required value: kind and name are required")
	}
	if e.Type != corev1.EventTypeNormal && e.Type != corev1.EventTypeWarning {
		errs = append(errs, "type: Unsupported value: must be Normal or Warning")
	}
	return errs
}
