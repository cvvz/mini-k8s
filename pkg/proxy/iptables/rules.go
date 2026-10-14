// Package iptables is kube-proxy's iptables mode: it turns Services and
// Endpoints into NAT rules in the node's network namespace, with the chain
// layout, names and comments of the real kube-proxy:
//
//	PREROUTING/OUTPUT -> KUBE-SERVICES -> KUBE-SVC-<hash>  (one per Service port)
//	                                   -> KUBE-SEP-<hash>  (one per endpoint, picked at random)
//	                                   -> DNAT to the Pod
//	KUBE-SERVICES (last rule) -> KUBE-NODEPORTS -> KUBE-EXT-<hash> -> KUBE-SVC-<hash>
//	POSTROUTING -> KUBE-POSTROUTING: MASQUERADE packets marked by KUBE-MARK-MASQ
//
// Service ports without ready endpoints are rejected in the filter table so
// clients fail fast instead of timing out.
package iptables

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// Chains kube-proxy owns outright.
const (
	kubeServicesChain         = "KUBE-SERVICES"
	kubeExternalServicesChain = "KUBE-EXTERNAL-SERVICES"
	kubeNodePortsChain        = "KUBE-NODEPORTS"
	kubePostroutingChain      = "KUBE-POSTROUTING"
	kubeMarkMasqChain         = "KUBE-MARK-MASQ"
	kubeForwardChain          = "KUBE-FORWARD"
	masqueradeMark            = "0x4000"
)

// jump is a rule in a built-in chain that hands traffic to kube-proxy.
type jump struct {
	table, chain string
	rule         []string
}

// jumps are kept in place with iptables -C/-I outside of iptables-restore,
// which only rewrites kube-proxy's own chains.
var jumps = []jump{
	{"nat", "PREROUTING", []string{"-m", "comment", "--comment", "kubernetes service portals", "-j", kubeServicesChain}},
	{"nat", "OUTPUT", []string{"-m", "comment", "--comment", "kubernetes service portals", "-j", kubeServicesChain}},
	{"nat", "POSTROUTING", []string{"-m", "comment", "--comment", "kubernetes postrouting rules", "-j", kubePostroutingChain}},
	{"filter", "INPUT", []string{"-m", "conntrack", "--ctstate", "NEW", "-m", "comment", "--comment", "kubernetes externally-visible service portals", "-j", kubeExternalServicesChain}},
	{"filter", "FORWARD", []string{"-m", "comment", "--comment", "kubernetes forwarding rules", "-j", kubeForwardChain}},
	{"filter", "FORWARD", []string{"-m", "conntrack", "--ctstate", "NEW", "-m", "comment", "--comment", "kubernetes service portals", "-j", kubeServicesChain}},
	{"filter", "FORWARD", []string{"-m", "conntrack", "--ctstate", "NEW", "-m", "comment", "--comment", "kubernetes externally-visible service portals", "-j", kubeExternalServicesChain}},
	{"filter", "OUTPUT", []string{"-m", "conntrack", "--ctstate", "NEW", "-m", "comment", "--comment", "kubernetes service portals", "-j", kubeServicesChain}},
}

// servicePortName is "namespace/name:port", or "namespace/name" for an
// unnamed port, as in kube-proxy's chain hashes and comments.
func servicePortName(svc *corev1.Service, port *corev1.ServicePort) string {
	if port.Name == "" {
		return svc.Namespace + "/" + svc.Name
	}
	return svc.Namespace + "/" + svc.Name + ":" + port.Name
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base32.StdEncoding.EncodeToString(sum[:])[:16]
}

// ServiceChain, ExternalChain and EndpointChain name the per-port and
// per-endpoint chains like kube-proxy does.
func ServiceChain(spn, protocol string) string {
	return "KUBE-SVC-" + hash(spn+strings.ToLower(protocol))
}
func ExternalChain(spn, protocol string) string {
	return "KUBE-EXT-" + hash(spn+strings.ToLower(protocol))
}
func EndpointChain(spn, protocol, endpoint string) string {
	return "KUBE-SEP-" + hash(spn+strings.ToLower(protocol)+endpoint)
}

// probability is kube-proxy's computeProbability.
func probability(n int) string { return fmt.Sprintf("%0.10f", 1.0/float64(n)) }

// endpointsFor returns the ready endpoints ("ip:port") of a Service port,
// sorted.
func endpointsFor(ep *corev1.Endpoints, port *corev1.ServicePort) []string {
	if ep == nil {
		return nil
	}
	var out []string
	for _, ss := range ep.Subsets {
		for _, p := range ss.Ports {
			if p.Name != port.Name || p.Protocol != port.Protocol {
				continue
			}
			for _, a := range ss.Addresses {
				out = append(out, net.JoinHostPort(a.IP, strconv.Itoa(int(p.Port))))
			}
		}
	}
	sort.Strings(out)
	return out
}

// buffer accumulates the chains and rules of one table.
type buffer struct {
	chains []string
	rules  bytes.Buffer
}

func (b *buffer) chain(name string) { b.chains = append(b.chains, name) }

func (b *buffer) rule(chain string, args ...string) {
	b.rules.WriteString("-A " + chain)
	for _, a := range args {
		if strings.ContainsAny(a, " \"") || a == "" {
			a = strconv.Quote(a)
		}
		b.rules.WriteString(" " + a)
	}
	b.rules.WriteString("\n")
}

func (b *buffer) write(out *bytes.Buffer, table string, stale []string) {
	out.WriteString("*" + table + "\n")
	for _, c := range b.chains {
		out.WriteString(":" + c + " - [0:0]\n")
	}
	// A stale chain is declared (which flushes it) before it is deleted, as
	// rules of other stale chains may still jump to it.
	for _, c := range stale {
		out.WriteString(":" + c + " - [0:0]\n")
	}
	out.Write(b.rules.Bytes())
	for _, c := range stale {
		out.WriteString("-X " + c + "\n")
	}
	out.WriteString("COMMIT\n")
}

// BuildRules returns the iptables-restore input (for --noflush) that
// implements services. existing lists the KUBE-SVC/SEP/EXT chains currently
// in the nat table; those no longer needed are deleted.
func BuildRules(services []*corev1.Service, endpoints []*corev1.Endpoints, clusterCIDR string, existing []string) []byte {
	eps := map[string]*corev1.Endpoints{}
	for _, e := range endpoints {
		eps[e.Namespace+"/"+e.Name] = e
	}
	sorted := append([]*corev1.Service(nil), services...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Namespace+"/"+sorted[i].Name < sorted[j].Namespace+"/"+sorted[j].Name
	})

	var filter, nat buffer
	filter.chain(kubeServicesChain)
	filter.chain(kubeExternalServicesChain)
	filter.chain(kubeForwardChain)
	filter.rule(kubeForwardChain, "-m", "comment", "--comment", "kubernetes forwarding rules", "-m", "mark", "--mark", masqueradeMark+"/"+masqueradeMark, "-j", "ACCEPT")
	filter.rule(kubeForwardChain, "-m", "comment", "--comment", "kubernetes forwarding conntrack rule", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT")

	for _, c := range []string{kubeServicesChain, kubeNodePortsChain, kubePostroutingChain, kubeMarkMasqChain} {
		nat.chain(c)
	}
	nat.rule(kubePostroutingChain, "-m", "mark", "!", "--mark", masqueradeMark+"/"+masqueradeMark, "-j", "RETURN")
	nat.rule(kubePostroutingChain, "-j", "MARK", "--xor-mark", masqueradeMark)
	nat.rule(kubePostroutingChain, "-m", "comment", "--comment", "kubernetes service traffic requiring SNAT", "-j", "MASQUERADE", "--random-fully")
	nat.rule(kubeMarkMasqChain, "-j", "MARK", "--or-mark", masqueradeMark)

	active := map[string]bool{}
	for _, svc := range sorted {
		if svc.Spec.ClusterIP == "" {
			continue // stored before cluster IPs existed; the API server's repair assigns one
		}
		for i := range svc.Spec.Ports {
			port := &svc.Spec.Ports[i]
			spn := servicePortName(svc, port)
			proto := strings.ToLower(port.Protocol)
			dst := svc.Spec.ClusterIP + "/32"
			dport := strconv.Itoa(int(port.Port))
			nodePort := ""
			if svc.Spec.Type == corev1.ServiceTypeNodePort && port.NodePort != 0 {
				nodePort = strconv.Itoa(int(port.NodePort))
			}
			targets := endpointsFor(eps[svc.Namespace+"/"+svc.Name], port)
			if len(targets) == 0 {
				filter.rule(kubeServicesChain, "-m", "comment", "--comment", spn+" has no endpoints", "-d", dst,
					"-p", proto, "-m", proto, "--dport", dport, "-j", "REJECT", "--reject-with", "icmp-port-unreachable")
				if nodePort != "" {
					filter.rule(kubeExternalServicesChain, "-m", "comment", "--comment", spn+" has no endpoints", "-m", "addrtype",
						"--dst-type", "LOCAL", "-p", proto, "-m", proto, "--dport", nodePort, "-j", "REJECT", "--reject-with", "icmp-port-unreachable")
				}
				continue
			}
			svcChain := ServiceChain(spn, port.Protocol)
			nat.chain(svcChain)
			active[svcChain] = true
			nat.rule(kubeServicesChain, "-m", "comment", "--comment", spn+" cluster IP", "-d", dst,
				"-p", proto, "-m", proto, "--dport", dport, "-j", svcChain)
			// Traffic from outside the Pod network (nodes, hostNetwork Pods)
			// must return through this node.
			nat.rule(svcChain, "-m", "comment", "--comment", spn+" cluster IP", "!", "-s", clusterCIDR, "-d", dst,
				"-p", proto, "-m", proto, "--dport", dport, "-j", kubeMarkMasqChain)
			if nodePort != "" {
				extChain := ExternalChain(spn, port.Protocol)
				nat.chain(extChain)
				active[extChain] = true
				nat.rule(kubeNodePortsChain, "-m", "comment", "--comment", spn, "-p", proto, "-m", proto, "--dport", nodePort, "-j", extChain)
				nat.rule(extChain, "-m", "comment", "--comment", "masquerade traffic for "+spn+" external destinations", "-j", kubeMarkMasqChain)
				nat.rule(extChain, "-j", svcChain)
			}
			affinity := ""
			if svc.Spec.SessionAffinity == corev1.ServiceAffinityClientIP {
				affinity = strconv.Itoa(int(corev1.DefaultClientIPServiceAffinitySeconds))
				if c := svc.Spec.SessionAffinityConfig; c != nil && c.ClientIP != nil && c.ClientIP.TimeoutSeconds != nil {
					affinity = strconv.Itoa(int(*c.ClientIP.TimeoutSeconds))
				}
			}
			sepChains := make([]string, len(targets))
			for j, target := range targets {
				sepChains[j] = EndpointChain(spn, port.Protocol, target)
			}
			if affinity != "" {
				// Clients seen recently go back to the endpoint they had.
				for j, target := range targets {
					nat.rule(svcChain, "-m", "comment", "--comment", spn+" -> "+target, "-m", "recent", "--name", sepChains[j],
						"--mask", "255.255.255.255", "--rsource", "--rcheck", "--seconds", affinity, "--reap", "-j", sepChains[j])
				}
			}
			for j, target := range targets {
				args := []string{"-m", "comment", "--comment", spn + " -> " + target}
				if j < len(targets)-1 {
					args = append(args, "-m", "statistic", "--mode", "random", "--probability", probability(len(targets)-j))
				}
				nat.rule(svcChain, append(args, "-j", sepChains[j])...)
			}
			for j, target := range targets {
				sep := sepChains[j]
				nat.chain(sep)
				active[sep] = true
				ip, _, _ := net.SplitHostPort(target)
				// Hairpin: a Pod reaching itself through its Service.
				nat.rule(sep, "-m", "comment", "--comment", spn, "-s", ip+"/32", "-j", kubeMarkMasqChain)
				dnat := []string{"-m", "comment", "--comment", spn}
				if affinity != "" {
					dnat = append(dnat, "-m", "recent", "--name", sep, "--mask", "255.255.255.255", "--rsource", "--set")
				}
				nat.rule(sep, append(dnat, "-m", proto, "-p", proto, "-j", "DNAT", "--to-destination", target)...)
			}
		}
	}
	nat.rule(kubeServicesChain, "-m", "comment", "--comment", "kubernetes service nodeports; NOTE: this must be the last rule in this chain",
		"-m", "addrtype", "--dst-type", "LOCAL", "-j", kubeNodePortsChain)

	var stale []string
	for _, c := range existing {
		if !active[c] && (strings.HasPrefix(c, "KUBE-SVC-") || strings.HasPrefix(c, "KUBE-SEP-") || strings.HasPrefix(c, "KUBE-EXT-")) {
			stale = append(stale, c)
		}
	}
	sort.Strings(stale)
	var out bytes.Buffer
	filter.write(&out, "filter", nil)
	nat.write(&out, "nat", stale)
	return out.Bytes()
}
