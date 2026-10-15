package kuberuntime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// ensureHostsFile writes the Pod's /etc/hosts like the kubelet does and
// returns its path; host-network Pods keep the node's (""). The file names
// the Pod by its hostname and, with a subdomain, by its fully qualified
// name <hostname>.<subdomain>.<namespace>.svc.<cluster domain>.
func (m *Manager) ensureHostsFile(pod *corev1.Pod, ip string) (string, error) {
	if pod.Spec.HostNetwork || ip == "" {
		return "", nil
	}
	path := filepath.Join(m.podDir(pod.UID), "etc-hosts")
	content := HostsFileContent(pod, ip, m.ClusterDomain)
	if current, err := os.ReadFile(path); err == nil && string(current) == content {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write hosts file: %w", err)
	}
	return path, nil
}

// HostsFileContent is the kubelet's managed hosts file.
func HostsFileContent(pod *corev1.Pod, ip, clusterDomain string) string {
	var b strings.Builder
	b.WriteString("# Kubernetes-managed hosts file.\n")
	b.WriteString("127.0.0.1\tlocalhost\n")
	b.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n")
	b.WriteString("fe00::0\tip6-localnet\n")
	b.WriteString("fe00::0\tip6-mcastprefix\n")
	b.WriteString("fe00::1\tip6-allnodes\n")
	b.WriteString("fe00::2\tip6-allrouters\n")
	hostname := podHostname(pod)
	if pod.Spec.Subdomain != "" && clusterDomain != "" {
		fmt.Fprintf(&b, "%s\t%s.%s.%s.svc.%s\t%s\n", ip, hostname, pod.Spec.Subdomain, pod.Namespace, clusterDomain, hostname)
	} else {
		fmt.Fprintf(&b, "%s\t%s\n", ip, hostname)
	}
	return b.String()
}
