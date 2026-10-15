// Package validation validates core API objects. Errors use the
// "field.path: Reason: detail" form returned to clients as Status causes.
package validation

import (
	"fmt"
	"net"
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
	for i, t := range spec.Tolerations {
		errs = append(errs, validateToleration(fmt.Sprintf("%s.tolerations[%d]", path, i), t)...)
	}
	for _, f := range [][2]string{{"hostname", spec.Hostname}, {"subdomain", spec.Subdomain}} {
		if f[1] != "" && !validation.IsDNS1123Label(f[1]) {
			errs = append(errs, fmt.Sprintf("%s.%s: Invalid value: %q: must be a DNS label", path, f[0], f[1]))
		}
	}
	errs = append(errs, validateDNS(path, spec)...)
	errs = append(errs, validateScheduling(path, spec)...)
	return errs
}

// validateDNS checks dnsPolicy and dnsConfig like Kubernetes (up to three
// nameservers and 32 search domains).
func validateDNS(path string, spec *corev1.PodSpec) []string {
	var errs []string
	switch spec.DNSPolicy {
	case corev1.DNSClusterFirst, corev1.DNSClusterFirstWithHostNet, corev1.DNSDefault:
	case corev1.DNSNone:
		if spec.DNSConfig == nil || len(spec.DNSConfig.Nameservers) == 0 {
			errs = append(errs, path+".dnsConfig: Required value: must provide at least one DNS nameserver when `dnsPolicy` is None")
		}
	default:
		errs = append(errs, fmt.Sprintf(`%s.dnsPolicy: Unsupported value: %q: supported values: "ClusterFirstWithHostNet", "ClusterFirst", "Default", "None"`, path, spec.DNSPolicy))
	}
	c := spec.DNSConfig
	if c == nil {
		return errs
	}
	if len(c.Nameservers) > 3 {
		errs = append(errs, fmt.Sprintf("%s.dnsConfig.nameservers: Invalid value: must not have more than 3 nameservers", path))
	}
	for i, ns := range c.Nameservers {
		if net.ParseIP(ns) == nil {
			errs = append(errs, fmt.Sprintf("%s.dnsConfig.nameservers[%d]: Invalid value: %q: must be a valid IP address", path, i, ns))
		}
	}
	if len(c.Searches) > 32 {
		errs = append(errs, fmt.Sprintf("%s.dnsConfig.searches: Invalid value: must not have more than 32 search paths", path))
	}
	for i, s := range c.Searches {
		if !validation.IsDNS1123Subdomain(strings.TrimSuffix(s, ".")) {
			errs = append(errs, fmt.Sprintf("%s.dnsConfig.searches[%d]: Invalid value: %q: must be a DNS subdomain", path, i, s))
		}
	}
	for i, o := range c.Options {
		if o.Name == "" {
			errs = append(errs, fmt.Sprintf("%s.dnsConfig.options[%d].name: Required value", path, i))
		}
	}
	return errs
}

var effects = map[string]bool{corev1.TaintEffectNoSchedule: true, corev1.TaintEffectPreferNoSchedule: true, corev1.TaintEffectNoExecute: true}

func validateToleration(path string, t corev1.Toleration) []string {
	var errs []string
	switch t.Operator {
	case "", corev1.TolerationOpEqual:
		if t.Key == "" {
			errs = append(errs, path+".operator: Invalid value: operator must be Exists when `key` is empty, which means \"match all values and all keys\"")
		}
	case corev1.TolerationOpExists:
		if t.Value != "" {
			errs = append(errs, path+".operator: Invalid value: value must be empty when `operator` is 'Exists'")
		}
	default:
		errs = append(errs, fmt.Sprintf(`%s.operator: Unsupported value: %q: supported values: "Equal", "Exists"`, path, t.Operator))
	}
	if t.Effect != "" && !effects[t.Effect] {
		errs = append(errs, fmt.Sprintf(`%s.effect: Unsupported value: %q: supported values: "NoSchedule", "PreferNoSchedule", "NoExecute"`, path, t.Effect))
	}
	return errs
}

// ValidateTaints checks node taints: a qualified key, a known effect and no
// duplicate key/effect pairs.
func ValidateTaints(path string, taints []corev1.Taint) []string {
	var errs []string
	seen := map[string]bool{}
	for i, t := range taints {
		p := fmt.Sprintf("%s[%d]", path, i)
		if err := labels.ValidateLabels(map[string]string{t.Key: t.Value}); err != nil || t.Key == "" {
			errs = append(errs, fmt.Sprintf("%s: Invalid value: taint key and value must be a valid label key and value", p))
		}
		if !effects[t.Effect] {
			errs = append(errs, fmt.Sprintf(`%s.effect: Unsupported value: %q: supported values: "NoSchedule", "PreferNoSchedule", "NoExecute"`, p, t.Effect))
		}
		if seen[t.Key+":"+t.Effect] {
			errs = append(errs, fmt.Sprintf("%s: Duplicate value: taints must be unique by key and effect", p))
		}
		seen[t.Key+":"+t.Effect] = true
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
		switch c.ImagePullPolicy {
		case corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
		default:
			errs = append(errs, fmt.Sprintf(`%s.imagePullPolicy: Unsupported value: %q: supported values: "Always", "IfNotPresent", "Never"`, p, c.ImagePullPolicy))
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
			errs = append(errs, validateProtocol(p+".ports.protocol", port.Protocol)...)
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
	if err := labels.ValidateLabels(spec.Selector); err != nil {
		errs = append(errs, "spec.selector: Invalid value: "+err.Error())
	}
	switch spec.Type {
	case corev1.ServiceTypeClusterIP, corev1.ServiceTypeNodePort:
		if spec.ExternalName != "" {
			errs = append(errs, fmt.Sprintf("spec.externalName: Forbidden: may not be set when `type` is '%s'", spec.Type))
		}
	case corev1.ServiceTypeExternalName:
		if spec.ClusterIP != "" {
			errs = append(errs, fmt.Sprintf("spec.clusterIP: Invalid value: %q: may not be set for ExternalName services", spec.ClusterIP))
		}
		if spec.ExternalName == "" {
			errs = append(errs, "spec.externalName: Required value")
		} else if !validation.IsDNS1123Subdomain(strings.TrimSuffix(spec.ExternalName, ".")) {
			errs = append(errs, fmt.Sprintf("spec.externalName: Invalid value: %q: must be a DNS subdomain", spec.ExternalName))
		}
	default:
		errs = append(errs, fmt.Sprintf(`spec.type: Unsupported value: %q: supported values: "ClusterIP", "ExternalName", "NodePort"`, spec.Type))
	}
	switch {
	case spec.ClusterIP == corev1.ClusterIPNone:
		if spec.Type != corev1.ServiceTypeClusterIP {
			errs = append(errs, fmt.Sprintf("spec.clusterIP: Invalid value: \"None\": may not be set to 'None' for %s services", spec.Type))
		}
	case spec.ClusterIP != "" && net.ParseIP(spec.ClusterIP).To4() == nil:
		errs = append(errs, fmt.Sprintf("spec.clusterIP: Invalid value: %q: must be a valid IPv4 address or None", spec.ClusterIP))
	}
	switch spec.SessionAffinity {
	case corev1.ServiceAffinityNone:
		if spec.SessionAffinityConfig != nil {
			errs = append(errs, "spec.sessionAffinityConfig: Invalid value: must not be set when sessionAffinity is None")
		}
	case corev1.ServiceAffinityClientIP:
		if t := spec.SessionAffinityConfig.ClientIP.TimeoutSeconds; *t < 1 || *t > 86400 {
			errs = append(errs, fmt.Sprintf("spec.sessionAffinityConfig.clientIP.timeoutSeconds: Invalid value: %d: must be greater than 0 and less than 86400", *t))
		}
	default:
		errs = append(errs, fmt.Sprintf(`spec.sessionAffinity: Unsupported value: %q: supported values: "ClientIP", "None"`, spec.SessionAffinity))
	}
	if len(spec.Ports) == 0 && spec.Type != corev1.ServiceTypeExternalName && spec.ClusterIP != corev1.ClusterIPNone {
		errs = append(errs, "spec.ports: Required value")
	}
	names, ports, nodePorts := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, p := range spec.Ports {
		path := fmt.Sprintf("spec.ports[%d]", i)
		switch {
		case p.Name == "" && len(spec.Ports) > 1:
			errs = append(errs, path+".name: Required value")
		case p.Name != "" && !validation.IsDNS1123Label(p.Name):
			errs = append(errs, fmt.Sprintf("%s.name: Invalid value: %q: must be a DNS label", path, p.Name))
		case names[p.Name]:
			errs = append(errs, fmt.Sprintf("%s.name: Duplicate value: %q", path, p.Name))
		}
		names[p.Name] = true
		errs = append(errs, validatePort(path+".port", false, p.Port, "")...)
		errs = append(errs, validateProtocol(path+".protocol", p.Protocol)...)
		errs = append(errs, validatePort(path+".targetPort", p.TargetPort.IsString, p.TargetPort.IntVal, p.TargetPort.StrVal)...)
		if key := fmt.Sprintf("%d/%s", p.Port, p.Protocol); ports[key] {
			errs = append(errs, fmt.Sprintf("%s: Duplicate value: %q", path, key))
		} else {
			ports[key] = true
		}
		if p.NodePort == 0 {
			continue
		}
		if spec.Type != corev1.ServiceTypeNodePort {
			errs = append(errs, fmt.Sprintf("%s.nodePort: Forbidden: may not be used when `type` is '%s'", path, spec.Type))
		}
		if key := fmt.Sprintf("%d/%s", p.NodePort, p.Protocol); nodePorts[key] {
			errs = append(errs, fmt.Sprintf("%s.nodePort: Duplicate value: %q", path, key))
		} else {
			nodePorts[key] = true
		}
	}
	return errs
}

// ValidateServiceUpdate rejects changes to the cluster IP, except when the
// Service becomes or stops being an ExternalName.
func ValidateServiceUpdate(spec, old *corev1.ServiceSpec) []string {
	external := spec.Type == corev1.ServiceTypeExternalName || old.Type == corev1.ServiceTypeExternalName
	if old.ClusterIP != "" && spec.ClusterIP != old.ClusterIP && !external {
		return []string{fmt.Sprintf("spec.clusterIP: Invalid value: %q: may not change once set", spec.ClusterIP)}
	}
	return nil
}

func validateProtocol(path, protocol string) []string {
	if protocol != corev1.ProtocolTCP && protocol != corev1.ProtocolUDP {
		return []string{fmt.Sprintf(`%s: Unsupported value: %q: supported values: "TCP", "UDP"`, path, protocol)}
	}
	return nil
}

// ValidateEndpoints validates Endpoints, which users may write for
// Services without a selector.
func ValidateEndpoints(e *corev1.Endpoints) []string {
	var errs []string
	for i, ss := range e.Subsets {
		path := fmt.Sprintf("subsets[%d]", i)
		if len(ss.Addresses) == 0 && len(ss.NotReadyAddresses) == 0 {
			errs = append(errs, path+": Required value: must specify `addresses` or `notReadyAddresses`")
		}
		for _, group := range []struct {
			field string
			list  []corev1.EndpointAddress
		}{{"addresses", ss.Addresses}, {"notReadyAddresses", ss.NotReadyAddresses}} {
			for j, a := range group.list {
				ap := fmt.Sprintf("%s.%s[%d]", path, group.field, j)
				if net.ParseIP(a.IP).To4() == nil {
					errs = append(errs, fmt.Sprintf("%s.ip: Invalid value: %q: must be a valid IPv4 address", ap, a.IP))
				}
				if a.Hostname != "" && !validation.IsDNS1123Label(a.Hostname) {
					errs = append(errs, fmt.Sprintf("%s.hostname: Invalid value: %q: must be a DNS label", ap, a.Hostname))
				}
			}
		}
		for j, p := range ss.Ports {
			pp := fmt.Sprintf("%s.ports[%d]", path, j)
			if p.Name == "" && len(ss.Ports) > 1 {
				errs = append(errs, pp+".name: Required value")
			} else if p.Name != "" && !validation.IsDNS1123Label(p.Name) {
				errs = append(errs, fmt.Sprintf("%s.name: Invalid value: %q: must be a DNS label", pp, p.Name))
			}
			errs = append(errs, validatePort(pp+".port", false, p.Port, "")...)
			errs = append(errs, validateProtocol(pp+".protocol", p.Protocol)...)
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
