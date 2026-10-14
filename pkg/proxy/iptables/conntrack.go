package iptables

import (
	"log"
	"net"
	"strconv"
	"strings"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// udpPort is where clients send the UDP packets of a Service port.
type udpPort struct {
	clusterIP string
	nodePort  int32
}

// udpEndpoints maps every UDP Service port to the IPs of its ready endpoints.
func udpEndpoints(services []*corev1.Service, endpoints []*corev1.Endpoints) map[udpPort]map[string]bool {
	eps := map[string]*corev1.Endpoints{}
	for _, e := range endpoints {
		eps[e.Namespace+"/"+e.Name] = e
	}
	out := map[udpPort]map[string]bool{}
	for _, svc := range services {
		if svc.Spec.ClusterIP == "" {
			continue
		}
		for i := range svc.Spec.Ports {
			port := &svc.Spec.Ports[i]
			if port.Protocol != corev1.ProtocolUDP {
				continue
			}
			key := udpPort{clusterIP: svc.Spec.ClusterIP}
			if svc.Spec.Type == corev1.ServiceTypeNodePort {
				key.nodePort = port.NodePort
			}
			if out[key] == nil {
				out[key] = map[string]bool{}
			}
			for _, target := range endpointsFor(eps[svc.Namespace+"/"+svc.Name], port) {
				ip, _, _ := net.SplitHostPort(target)
				out[key][ip] = true
			}
		}
	}
	return out
}

// clearStaleUDP deletes the conntrack entries iptables no longer decides,
// like kube-proxy: UDP has no connection end, so a client that keeps
// sending keeps its entry alive and would stick to a removed endpoint, or
// keep being rejected after the Service gained endpoints.
func (p *Proxier) clearStaleUDP(current map[udpPort]map[string]bool) {
	if p.udp != nil {
		for key, ips := range current {
			if len(ips) > 0 && len(p.udp[key]) == 0 {
				p.conntrack("--orig-dst", key.clusterIP)
				if key.nodePort != 0 {
					p.conntrack("--orig-port-dst", strconv.Itoa(int(key.nodePort)))
				}
			}
		}
		for key, ips := range p.udp {
			for ip := range ips {
				if current[key][ip] {
					continue
				}
				p.conntrack("--orig-dst", key.clusterIP, "--dst-nat", ip)
				if key.nodePort != 0 {
					p.conntrack("--orig-port-dst", strconv.Itoa(int(key.nodePort)), "--dst-nat", ip)
				}
			}
		}
	}
	p.udp = current
}

func (p *Proxier) conntrack(filter ...string) {
	_, err := p.Exec("conntrack", append([]string{"-D", "-p", "udp"}, filter...)...)
	// conntrack exits with 1 when no entry matched.
	if err != nil && !strings.Contains(err.Error(), "0 flow entries") {
		log.Printf("kube-proxy: %v", err)
	}
}
