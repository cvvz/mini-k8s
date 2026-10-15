// Package envvars builds the Service environment variables the kubelet
// gives every container ({SVC}_SERVICE_HOST and the Docker link variables),
// like k8s.io/kubernetes/pkg/kubelet/envvars.
package envvars

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// ForPod picks the Services whose variables a Pod gets, like the kubelet:
// the kubernetes Service of the default namespace always, and the Services
// of the Pod's namespace unless enableServiceLinks is false. Services
// without a cluster IP have none.
func ForPod(pod *corev1.Pod, services []*corev1.Service) []corev1.EnvVar {
	links := pod.Spec.EnableServiceLinks == nil || *pod.Spec.EnableServiceLinks
	byName := map[string]*corev1.Service{}
	for _, s := range services {
		if !corev1.IsServiceIPSet(s) {
			continue
		}
		if s.Namespace == pod.Namespace && links {
			byName[s.Name] = s
		}
	}
	for _, s := range services {
		if s.Namespace == corev1.NamespaceDefault && s.Name == "kubernetes" && corev1.IsServiceIPSet(s) {
			if _, ok := byName[s.Name]; !ok {
				byName[s.Name] = s
			}
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []corev1.EnvVar
	for _, n := range names {
		out = append(out, FromService(byName[n])...)
	}
	return out
}

// FromService returns the variables of one Service.
func FromService(s *corev1.Service) []corev1.EnvVar {
	if len(s.Spec.Ports) == 0 {
		return nil
	}
	prefix := name(s.Name)
	out := []corev1.EnvVar{
		{Name: prefix + "_SERVICE_HOST", Value: s.Spec.ClusterIP},
		{Name: prefix + "_SERVICE_PORT", Value: strconv.Itoa(int(s.Spec.Ports[0].Port))},
	}
	for _, p := range s.Spec.Ports {
		if p.Name != "" {
			out = append(out, corev1.EnvVar{Name: prefix + "_SERVICE_PORT_" + name(p.Name), Value: strconv.Itoa(int(p.Port))})
		}
	}
	for i, p := range s.Spec.Ports {
		protocol := strings.ToLower(p.Protocol)
		if protocol == "" {
			protocol = "tcp"
		}
		url := fmt.Sprintf("%s://%s", protocol, net.JoinHostPort(s.Spec.ClusterIP, strconv.Itoa(int(p.Port))))
		if i == 0 {
			out = append(out, corev1.EnvVar{Name: prefix + "_PORT", Value: url}) // Docker special-cases the first port
		}
		portPrefix := fmt.Sprintf("%s_PORT_%d_%s", prefix, p.Port, strings.ToUpper(protocol))
		out = append(out,
			corev1.EnvVar{Name: portPrefix, Value: url},
			corev1.EnvVar{Name: portPrefix + "_PROTO", Value: protocol},
			corev1.EnvVar{Name: portPrefix + "_PORT", Value: strconv.Itoa(int(p.Port))},
			corev1.EnvVar{Name: portPrefix + "_ADDR", Value: s.Spec.ClusterIP})
	}
	return out
}

func name(s string) string { return strings.ToUpper(strings.ReplaceAll(s, "-", "_")) }
