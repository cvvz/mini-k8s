// Package validation validates core API objects. Errors use the
// "field.path: Reason: detail" form returned to clients as Status causes.
package validation

import (
	"fmt"
	"path/filepath"
	"strings"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/labels"
	"mini-k8s/pkg/util/validation"
)

// ValidatePodSpec validates a Pod spec found at path.
func ValidatePodSpec(path string, spec *corev1.PodSpec) []string {
	var errs []string
	if len(spec.Containers) == 0 {
		errs = append(errs, path+".containers: Required value")
	}
	switch spec.RestartPolicy {
	case corev1.RestartPolicyAlways, corev1.RestartPolicyOnFailure, corev1.RestartPolicyNever:
	default:
		errs = append(errs, fmt.Sprintf(`%s.restartPolicy: Unsupported value: %q: supported values: "Always", "OnFailure", "Never"`, path, spec.RestartPolicy))
	}
	volumes, verrs := validateVolumes(path+".volumes", spec.Volumes)
	errs = append(errs, verrs...)
	names := map[string]bool{}
	errs = append(errs, validateContainers(path+".initContainers", spec.InitContainers, names, true, volumes)...)
	errs = append(errs, validateContainers(path+".containers", spec.Containers, names, false, volumes)...)
	if g := spec.TerminationGracePeriodSeconds; g != nil && *g < 0 {
		errs = append(errs, path+".terminationGracePeriodSeconds: Invalid value: must be >= 0")
	}
	if spec.NodeName != "" && !validation.IsDNS1123Subdomain(spec.NodeName) {
		errs = append(errs, path+".nodeName: Invalid value: must be a DNS subdomain")
	}
	return errs
}

// validateContainers validates a container list; names must be unique
// across init and regular containers, so seen is shared between the calls.
// Init containers run to completion and may not have probes.
func validateContainers(path string, containers []corev1.Container, seen map[string]bool, init bool, volumes map[string]bool) []string {
	var errs []string
	for i, c := range containers {
		p := fmt.Sprintf("%s[%d]", path, i)
		if !validation.IsDNS1123Label(c.Name) {
			errs = append(errs, p+".name: Invalid value: must be a DNS label")
		} else if seen[c.Name] {
			errs = append(errs, fmt.Sprintf("%s.name: Duplicate value: %q", p, c.Name))
		}
		seen[c.Name] = true
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
		errs = append(errs, validateResources(p+".resources", c.Resources)...)
		mountPaths := map[string]bool{}
		for j, m := range c.VolumeMounts {
			mp := fmt.Sprintf("%s.volumeMounts[%d]", p, j)
			if !volumes[m.Name] {
				errs = append(errs, fmt.Sprintf("%s.name: Not found: %q", mp, m.Name))
			}
			if !strings.HasPrefix(m.MountPath, "/") {
				errs = append(errs, fmt.Sprintf("%s.mountPath: Invalid value: %q: must be an absolute path", mp, m.MountPath))
			} else if mountPaths[cleanPath(m.MountPath)] {
				errs = append(errs, fmt.Sprintf("%s.mountPath: Invalid value: %q: must be unique", mp, m.MountPath))
			}
			mountPaths[cleanPath(m.MountPath)] = true
		}
		for _, probe := range []struct {
			name  string
			probe *corev1.Probe
		}{{"livenessProbe", c.LivenessProbe}, {"readinessProbe", c.ReadinessProbe}, {"startupProbe", c.StartupProbe}} {
			if probe.probe == nil {
				continue
			}
			pp := p + "." + probe.name
			if init {
				errs = append(errs, pp+": Forbidden: may not be set for init containers")
				continue
			}
			errs = append(errs, validateProbe(pp, probe.probe, probe.name != "readinessProbe")...)
		}
	}
	return errs
}

// validateVolumes checks names and sources and returns the volume names.
func validateVolumes(path string, volumes []corev1.Volume) (map[string]bool, []string) {
	var errs []string
	names := map[string]bool{}
	for i, v := range volumes {
		p := fmt.Sprintf("%s[%d]", path, i)
		if !validation.IsDNS1123Label(v.Name) {
			errs = append(errs, p+".name: Invalid value: must be a DNS label")
		} else if names[v.Name] {
			errs = append(errs, fmt.Sprintf("%s.name: Duplicate value: %q", p, v.Name))
		}
		names[v.Name] = true
		sources := 0
		if e := v.EmptyDir; e != nil {
			sources++
			if e.Medium != "" && e.Medium != corev1.StorageMediumMemory {
				errs = append(errs, fmt.Sprintf(`%s.emptyDir.medium: Unsupported value: %q: supported values: "", "Memory"`, p, e.Medium))
			}
		}
		if h := v.HostPath; h != nil {
			sources++
			if !strings.HasPrefix(h.Path, "/") {
				errs = append(errs, fmt.Sprintf("%s.hostPath.path: Invalid value: %q: must be an absolute path", p, h.Path))
			}
			if h.Type != nil {
				switch *h.Type {
				case "", corev1.HostPathDirectoryOrCreate, corev1.HostPathDirectory, corev1.HostPathFileOrCreate, corev1.HostPathFile:
				default:
					errs = append(errs, fmt.Sprintf(`%s.hostPath.type: Unsupported value: %q: supported values: "", "DirectoryOrCreate", "Directory", "FileOrCreate", "File"`, p, *h.Type))
				}
			}
		}
		if sources != 1 {
			errs = append(errs, p+": Required value: must specify exactly one of emptyDir, hostPath")
		}
	}
	return names, errs
}

// cleanPath normalises a mount path for duplicate detection.
func cleanPath(p string) string { return filepath.Clean(p) }

// validateResources allows cpu and memory, with requests at most the limits.
func validateResources(path string, r corev1.ResourceRequirements) []string {
	var errs []string
	for _, list := range []struct {
		name string
		rl   corev1.ResourceList
	}{{"limits", r.Limits}, {"requests", r.Requests}} {
		for name := range list.rl {
			if name != corev1.ResourceCPU && name != corev1.ResourceMemory {
				errs = append(errs, fmt.Sprintf(`%s.%s[%s]: Unsupported value: %q: supported values: "cpu", "memory"`, path, list.name, name, name))
			}
		}
	}
	for name, request := range r.Requests {
		if limit, ok := r.Limits[name]; ok && request.Cmp(limit) > 0 {
			errs = append(errs, fmt.Sprintf("%s.requests: Invalid value: %q: must be less than or equal to %s limit of %s", path, request.String(), name, limit.String()))
		}
	}
	return errs
}

// validateProbe requires exactly one handler and sane timing; liveness and
// startup probes must succeed once to count as healthy.
func validateProbe(path string, p *corev1.Probe, successMustBeOne bool) []string {
	var errs []string
	handlers := 0
	if p.Exec != nil {
		handlers++
		if len(p.Exec.Command) == 0 {
			errs = append(errs, path+".exec.command: Required value")
		}
	}
	if p.HTTPGet != nil {
		handlers++
		errs = append(errs, validatePort(path+".httpGet.port", p.HTTPGet.Port.IsString, p.HTTPGet.Port.IntVal, p.HTTPGet.Port.StrVal)...)
		if p.HTTPGet.Scheme != "HTTP" {
			errs = append(errs, fmt.Sprintf(`%s.httpGet.scheme: Unsupported value: %q: supported values: "HTTP"`, path, p.HTTPGet.Scheme))
		}
	}
	if p.TCPSocket != nil {
		handlers++
		errs = append(errs, validatePort(path+".tcpSocket.port", p.TCPSocket.Port.IsString, p.TCPSocket.Port.IntVal, p.TCPSocket.Port.StrVal)...)
	}
	if handlers != 1 {
		errs = append(errs, path+": Required value: must specify exactly one of exec, httpGet, tcpSocket")
	}
	for _, f := range []struct {
		name string
		v    int32
		min  int32
	}{{"initialDelaySeconds", p.InitialDelaySeconds, 0}, {"timeoutSeconds", p.TimeoutSeconds, 1}, {"periodSeconds", p.PeriodSeconds, 1},
		{"successThreshold", p.SuccessThreshold, 1}, {"failureThreshold", p.FailureThreshold, 1}} {
		if f.v < f.min {
			errs = append(errs, fmt.Sprintf("%s.%s: Invalid value: %d: must be greater than or equal to %d", path, f.name, f.v, f.min))
		}
	}
	if successMustBeOne && p.SuccessThreshold != 1 {
		errs = append(errs, fmt.Sprintf("%s.successThreshold: Invalid value: %d: must be 1", path, p.SuccessThreshold))
	}
	return errs
}

func validatePort(path string, named bool, number int32, name string) []string {
	if named {
		if !validation.IsDNS1123Label(name) || len(name) > 15 {
			return []string{fmt.Sprintf("%s: Invalid value: %q: must be a port name of at most 15 lowercase letters, digits and '-'", path, name)}
		}
		return nil
	}
	if number < 1 || number > 65535 {
		return []string{fmt.Sprintf("%s: Invalid value: %d: must be between 1 and 65535, inclusive", path, number)}
	}
	return nil
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
