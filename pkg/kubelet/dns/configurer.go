package dns

import (
	"bufio"
	"os"
	"strings"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// Limits the kubelet applies to a Pod's resolver configuration.
const (
	maxNameservers = 3
	maxSearches    = 32
)

// Configurer computes a Pod's resolv.conf from its dnsPolicy and dnsConfig,
// like the kubelet's DNS configurer.
type Configurer struct {
	// ClusterDNS is the cluster DNS Service address (10.96.0.10); without
	// it ClusterFirst falls back to the node's resolver.
	ClusterDNS []string
	// ClusterDomain is the cluster's DNS domain (cluster.local).
	ClusterDomain string
	// HostServers and HostSearches are the node's resolver for Pods (the
	// node-local forwarder) and the node's search domains.
	HostServers  []string
	HostSearches []string
}

// For returns the sandbox DNS configuration of pod; nil keeps the node's
// /etc/resolv.conf, which is what host-network Pods with the default policy
// use.
func (c *Configurer) For(pod *corev1.Pod) *runtimeapi.DNSConfig {
	var cfg *runtimeapi.DNSConfig
	switch policy := pod.Spec.DNSPolicy; {
	case policy == corev1.DNSNone:
		cfg = &runtimeapi.DNSConfig{}
	case policy == corev1.DNSDefault || len(c.ClusterDNS) == 0 ||
		(pod.Spec.HostNetwork && policy != corev1.DNSClusterFirstWithHostNet):
		if pod.Spec.HostNetwork && pod.Spec.DNSConfig == nil {
			return nil
		}
		cfg = &runtimeapi.DNSConfig{Servers: c.HostServers, Searches: c.HostSearches}
	default: // ClusterFirst, ClusterFirstWithHostNet
		domain := c.ClusterDomain
		cfg = &runtimeapi.DNSConfig{
			Servers:  c.ClusterDNS,
			Searches: append([]string{pod.Namespace + ".svc." + domain, "svc." + domain, domain}, c.HostSearches...),
			Options:  []string{"ndots:5"},
		}
	}
	if extra := pod.Spec.DNSConfig; extra != nil {
		cfg.Servers = appendUnique(cfg.Servers, extra.Nameservers...)
		cfg.Searches = appendUnique(cfg.Searches, extra.Searches...)
		cfg.Options = mergeOptions(cfg.Options, extra.Options)
	}
	if len(cfg.Servers) > maxNameservers {
		cfg.Servers = cfg.Servers[:maxNameservers]
	}
	if len(cfg.Searches) > maxSearches {
		cfg.Searches = cfg.Searches[:maxSearches]
	}
	return cfg
}

func appendUnique(list []string, more ...string) []string {
	out := append([]string(nil), list...)
	seen := map[string]bool{}
	for _, s := range out {
		seen[s] = true
	}
	for _, s := range more {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// mergeOptions overrides options by name ("ndots:5" with ndots=2 becomes
// "ndots:2").
func mergeOptions(base []string, extra []corev1.PodDNSConfigOption) []string {
	var names []string
	values := map[string]string{}
	add := func(name, value string) {
		if _, ok := values[name]; !ok {
			names = append(names, name)
		}
		values[name] = value
	}
	for _, o := range base {
		name, value, _ := strings.Cut(o, ":")
		add(name, value)
	}
	for _, o := range extra {
		value := ""
		if o.Value != nil {
			value = *o.Value
		}
		add(o.Name, value)
	}
	var out []string
	for _, n := range names {
		if values[n] != "" {
			n += ":" + values[n]
		}
		out = append(out, n)
	}
	return out
}

// Searches returns the search domains of a resolv.conf file.
func Searches(resolvConf string) ([]string, error) {
	f, err := os.Open(resolvConf)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if fields := strings.Fields(scanner.Text()); len(fields) > 1 && (fields[0] == "search" || fields[0] == "domain") {
			out = fields[1:]
		}
	}
	return out, scanner.Err()
}
