package dns

import (
	"net"
	"sort"
	"strings"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// Lookup gives the zone read access to Services and Endpoints.
type Lookup interface {
	Service(namespace, name string) (*corev1.Service, bool)
	Endpoints(namespace, name string) (*corev1.Endpoints, bool)
}

// Zone answers for the names Kubernetes defines under the cluster domain:
//
//	<svc>.<ns>.svc.<domain>                     A: the cluster IP, or the ready endpoints of a headless Service;
//	                                            CNAME: an ExternalName Service's externalName
//	<host>.<svc>.<ns>.svc.<domain>              A: the headless endpoint with that hostname (or dashed IP)
//	_<port>._<proto>.<svc>.<ns>.svc.<domain>    SRV: the port, targeting the Service or each headless endpoint
//	<a-b-c-d>.<ns>.pod.<domain>                 A: that address
type Zone struct {
	Domain string // e.g. cluster.local
	TTL    uint32
	Data   Lookup
}

// Resolve answers q. inZone is false for names outside the cluster domain,
// which the server forwards. A known name with no record of the asked type
// gets an empty NOERROR answer (NODATA); an unknown name NXDOMAIN. For
// ExternalName Services, external is the name the CNAME points to.
func (z *Zone) Resolve(q Question) (answers []RR, rcode uint8, inZone bool, external string) {
	name := strings.ToLower(strings.TrimSuffix(q.Name, "."))
	domain := strings.ToLower(strings.TrimSuffix(z.Domain, "."))
	if name != domain && !strings.HasSuffix(name, "."+domain) {
		return nil, 0, false, ""
	}
	owner := name + "."
	labels := strings.Split(strings.TrimSuffix(strings.TrimSuffix(name, domain), "."), ".")
	if name == domain {
		if q.Type == TypeSOA || q.Type == TypeANY {
			return []RR{SOA(domain+".", z.TTL)}, RcodeSuccess, true, ""
		}
		return nil, RcodeSuccess, true, ""
	}
	wantA := q.Type == TypeA || q.Type == TypeANY
	n := len(labels)
	switch {
	case n == 3 && labels[2] == "pod":
		ip := net.ParseIP(strings.ReplaceAll(labels[0], "-", ".")).To4()
		if ip == nil {
			return nil, RcodeNameError, true, ""
		}
		if wantA {
			return []RR{A(owner, z.TTL, ip)}, RcodeSuccess, true, ""
		}
		return nil, RcodeSuccess, true, ""

	case n == 3 && labels[2] == "svc":
		svc, ok := z.Data.Service(labels[1], labels[0])
		if !ok {
			return nil, RcodeNameError, true, ""
		}
		switch {
		case svc.Spec.Type == corev1.ServiceTypeExternalName:
			if q.Type == TypeA || q.Type == TypeAAAA || q.Type == TypeCNAME || q.Type == TypeANY {
				target := strings.TrimSuffix(svc.Spec.ExternalName, ".") + "."
				return []RR{CNAME(owner, z.TTL, target)}, RcodeSuccess, true, target
			}
		case corev1.IsServiceIPSet(svc):
			if wantA {
				return []RR{A(owner, z.TTL, net.ParseIP(svc.Spec.ClusterIP))}, RcodeSuccess, true, ""
			}
		case wantA: // headless: every ready endpoint
			for _, ip := range z.readyIPs(svc) {
				answers = append(answers, A(owner, z.TTL, net.ParseIP(ip)))
			}
		}
		return answers, RcodeSuccess, true, ""

	case n == 4 && labels[3] == "svc":
		svc, ok := z.Data.Service(labels[2], labels[1])
		if !ok || svc.Spec.ClusterIP != corev1.ClusterIPNone {
			return nil, RcodeNameError, true, ""
		}
		for _, a := range z.readyAddresses(svc) {
			if endpointName(a) == labels[0] {
				if wantA {
					return []RR{A(owner, z.TTL, net.ParseIP(a.IP))}, RcodeSuccess, true, ""
				}
				return nil, RcodeSuccess, true, ""
			}
		}
		return nil, RcodeNameError, true, ""

	case n == 5 && labels[4] == "svc" && strings.HasPrefix(labels[0], "_") && strings.HasPrefix(labels[1], "_"):
		svc, ok := z.Data.Service(labels[3], labels[2])
		if !ok {
			return nil, RcodeNameError, true, ""
		}
		portName, protocol := labels[0][1:], strings.ToUpper(labels[1][1:])
		var port *corev1.ServicePort
		for i := range svc.Spec.Ports {
			if p := &svc.Spec.Ports[i]; p.Name == portName && p.Protocol == protocol {
				port = p
			}
		}
		if port == nil {
			return nil, RcodeNameError, true, ""
		}
		if q.Type != TypeSRV && q.Type != TypeANY {
			return nil, RcodeSuccess, true, ""
		}
		service := labels[2] + "." + labels[3] + ".svc." + domain + "."
		if corev1.IsServiceIPSet(svc) {
			return []RR{SRV(owner, z.TTL, 0, 100, uint16(port.Port), service)}, RcodeSuccess, true, ""
		}
		targets := z.srvTargets(svc, port)
		for _, t := range targets {
			answers = append(answers, SRV(owner, z.TTL, 0, uint16(100/len(targets)), t.port, t.name+"."+service))
		}
		return answers, RcodeSuccess, true, ""
	}
	return nil, RcodeNameError, true, ""
}

// endpointName is a headless endpoint's DNS label: its hostname, or its
// address with dashes.
func endpointName(a corev1.EndpointAddress) string {
	if a.Hostname != "" {
		return a.Hostname
	}
	return strings.ReplaceAll(a.IP, ".", "-")
}

func (z *Zone) readyAddresses(svc *corev1.Service) []corev1.EndpointAddress {
	ep, ok := z.Data.Endpoints(svc.Namespace, svc.Name)
	if !ok {
		return nil
	}
	var out []corev1.EndpointAddress
	for _, ss := range ep.Subsets {
		out = append(out, ss.Addresses...)
	}
	return out
}

func (z *Zone) readyIPs(svc *corev1.Service) []string {
	seen := map[string]bool{}
	var ips []string
	for _, a := range z.readyAddresses(svc) {
		if !seen[a.IP] {
			seen[a.IP] = true
			ips = append(ips, a.IP)
		}
	}
	sort.Strings(ips)
	return ips
}

type srvTarget struct {
	name string
	port uint16
}

// srvTargets lists the endpoints of a headless Service port with the port
// they listen on.
func (z *Zone) srvTargets(svc *corev1.Service, port *corev1.ServicePort) []srvTarget {
	ep, ok := z.Data.Endpoints(svc.Namespace, svc.Name)
	if !ok {
		return nil
	}
	var out []srvTarget
	for _, ss := range ep.Subsets {
		for _, p := range ss.Ports {
			if p.Name != port.Name || p.Protocol != port.Protocol {
				continue
			}
			for _, a := range ss.Addresses {
				out = append(out, srvTarget{name: endpointName(a), port: uint16(p.Port)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}
