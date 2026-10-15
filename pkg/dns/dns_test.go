package dns

import (
	"net"
	"reflect"
	"strings"
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

type fakeLookup struct {
	services  map[string]*corev1.Service
	endpoints map[string]*corev1.Endpoints
}

func (f fakeLookup) Service(ns, name string) (*corev1.Service, bool) {
	s, ok := f.services[ns+"/"+name]
	return s, ok
}

func (f fakeLookup) Endpoints(ns, name string) (*corev1.Endpoints, bool) {
	e, ok := f.endpoints[ns+"/"+name]
	return e, ok
}

func cluster() *Zone {
	svc := func(name string, spec corev1.ServiceSpec) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}, Spec: spec}
	}
	ports := []corev1.ServicePort{{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP}}
	return &Zone{Domain: "cluster.local", TTL: 30, Data: fakeLookup{
		services: map[string]*corev1.Service{
			"ns/web": svc("web", corev1.ServiceSpec{ClusterIP: "10.96.0.20", Ports: ports}),
			"ns/db":  svc("db", corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Ports: ports}),
			"ns/ext": svc("ext", corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "example.com"}),
		},
		endpoints: map[string]*corev1.Endpoints{
			"ns/db": {Subsets: []corev1.EndpointSubset{{
				Addresses: []corev1.EndpointAddress{{IP: "10.244.1.5", Hostname: "db-0"}, {IP: "10.244.2.7"}},
				Ports:     []corev1.EndpointPort{{Name: "http", Port: 8080, Protocol: corev1.ProtocolTCP}},
			}}},
		},
	}}
}

func strs(rrs []RR) []string {
	var out []string
	for _, rr := range rrs {
		out = append(out, rr.String())
	}
	return out
}

func TestZone(t *testing.T) {
	z := cluster()
	for _, tc := range []struct {
		name  string
		qtype uint16
		want  []string
		rcode uint8
	}{
		{"web.ns.svc.cluster.local.", TypeA, []string{"web.ns.svc.cluster.local. 30 A 10.96.0.20"}, RcodeSuccess},
		{"WEB.ns.svc.cluster.local", TypeA, []string{"web.ns.svc.cluster.local. 30 A 10.96.0.20"}, RcodeSuccess},
		{"web.ns.svc.cluster.local.", TypeAAAA, nil, RcodeSuccess},
		{"nope.ns.svc.cluster.local.", TypeA, nil, RcodeNameError},
		{"web.svc.cluster.local.", TypeA, nil, RcodeNameError},
		{"db.ns.svc.cluster.local.", TypeA, []string{"db.ns.svc.cluster.local. 30 A 10.244.1.5", "db.ns.svc.cluster.local. 30 A 10.244.2.7"}, RcodeSuccess},
		{"db-0.db.ns.svc.cluster.local.", TypeA, []string{"db-0.db.ns.svc.cluster.local. 30 A 10.244.1.5"}, RcodeSuccess},
		{"10-244-2-7.db.ns.svc.cluster.local.", TypeA, []string{"10-244-2-7.db.ns.svc.cluster.local. 30 A 10.244.2.7"}, RcodeSuccess},
		{"db-9.db.ns.svc.cluster.local.", TypeA, nil, RcodeNameError},
		{"x.web.ns.svc.cluster.local.", TypeA, nil, RcodeNameError}, // only headless Services have endpoint names
		{"_http._tcp.web.ns.svc.cluster.local.", TypeSRV, []string{"_http._tcp.web.ns.svc.cluster.local. 30 SRV 0 100 80 web.ns.svc.cluster.local."}, RcodeSuccess},
		{"_http._tcp.db.ns.svc.cluster.local.", TypeSRV, []string{
			"_http._tcp.db.ns.svc.cluster.local. 30 SRV 0 50 8080 10-244-2-7.db.ns.svc.cluster.local.",
			"_http._tcp.db.ns.svc.cluster.local. 30 SRV 0 50 8080 db-0.db.ns.svc.cluster.local."}, RcodeSuccess},
		{"_http._udp.web.ns.svc.cluster.local.", TypeSRV, nil, RcodeNameError},
		{"ext.ns.svc.cluster.local.", TypeA, []string{"ext.ns.svc.cluster.local. 30 CNAME example.com."}, RcodeSuccess},
		{"10-244-3-4.ns.pod.cluster.local.", TypeA, []string{"10-244-3-4.ns.pod.cluster.local. 30 A 10.244.3.4"}, RcodeSuccess},
		{"bad.ns.pod.cluster.local.", TypeA, nil, RcodeNameError},
		{"cluster.local.", TypeSOA, []string{"cluster.local. 30 SOA"}, RcodeSuccess},
	} {
		answers, rcode, inZone, _ := z.Resolve(Question{Name: tc.name, Type: tc.qtype, Class: ClassINET})
		if !inZone || rcode != tc.rcode || !reflect.DeepEqual(strs(answers), tc.want) {
			t.Errorf("%s %d: got %v rcode %d inZone %v, want %v rcode %d", tc.name, tc.qtype, strs(answers), rcode, inZone, tc.want, tc.rcode)
		}
	}
	if _, _, inZone, _ := z.Resolve(Question{Name: "example.com.", Type: TypeA}); inZone {
		t.Error("example.com is outside the zone")
	}
}

func query(name string, qtype uint16, edns uint16) []byte {
	m := &Message{ID: 7, RecursionDesired: true, Questions: []Question{{Name: name, Type: qtype, Class: ClassINET}}}
	if edns > 0 {
		m.Additional = []RR{{Name: ".", Type: TypeOPT, Class: edns}}
	}
	return m.Pack()
}

func TestServerAnswersForwardsAndTruncates(t *testing.T) {
	var forwarded []string
	upstream := func(q []byte) ([]byte, error) {
		m, err := Parse(q)
		if err != nil {
			return nil, err
		}
		forwarded = append(forwarded, m.Questions[0].Name)
		reply := &Message{ID: m.ID, Response: true, RecursionAvailable: true, Questions: m.Questions,
			Answers: []RR{A(m.Questions[0].Name, 60, net.ParseIP("93.184.215.14"))}}
		return reply.Pack(), nil
	}
	s := &Server{Zone: cluster(), Forward: upstream}

	m, err := Parse(s.Handle(query("web.ns.svc.cluster.local.", TypeA, 0), maxUDP))
	if err != nil || m.ID != 7 || !m.Response || !m.Authoritative || !m.RecursionAvailable || len(m.Answers) != 1 {
		t.Fatalf("local answer: %v %+v", err, m)
	}
	m, _ = Parse(s.Handle(query("nope.ns.svc.cluster.local.", TypeA, 0), maxUDP))
	if m.Rcode != RcodeNameError || len(m.Authority) != 1 || m.Authority[0].Type != TypeSOA {
		t.Fatalf("NXDOMAIN carries the SOA: %+v", m)
	}
	m, _ = Parse(s.Handle(query("example.org.", TypeA, 0), maxUDP))
	if m.Authoritative || strs(m.Answers)[0] != "example.org. 60 A 93.184.215.14" {
		t.Fatalf("forwarded answer: %+v", m)
	}
	// An ExternalName is followed upstream in the same answer.
	m, _ = Parse(s.Handle(query("ext.ns.svc.cluster.local.", TypeA, 0), maxUDP))
	if got := strings.Join(strs(m.Answers), "|"); got != "ext.ns.svc.cluster.local. 30 CNAME example.com.|example.com. 60 A 93.184.215.14" {
		t.Fatalf("ExternalName: %s", got)
	}
	// An ExternalName pointing into the cluster is answered locally.
	inCluster := cluster()
	inCluster.Data.(fakeLookup).services["ns/alias"] = &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "alias", Namespace: "ns"},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "web.ns.svc.cluster.local"}}
	s.Zone = inCluster
	m, _ = Parse(s.Handle(query("alias.ns.svc.cluster.local.", TypeA, 0), maxUDP))
	if got := strings.Join(strs(m.Answers), "|"); got != "alias.ns.svc.cluster.local. 30 CNAME web.ns.svc.cluster.local.|web.ns.svc.cluster.local. 30 A 10.96.0.20" {
		t.Fatalf("in-cluster ExternalName: %s", got)
	}
	if !reflect.DeepEqual(forwarded, []string{"example.org.", "example.com."}) {
		t.Fatalf("forwarded %v", forwarded)
	}

	// Too many addresses for 512 bytes: truncated over UDP, complete over
	// TCP or with a larger EDNS buffer.
	big := cluster()
	var addrs []corev1.EndpointAddress
	for i := range 40 {
		addrs = append(addrs, corev1.EndpointAddress{IP: net.IPv4(10, 244, 1, byte(i+1)).String()})
	}
	big.Data.(fakeLookup).endpoints["ns/db"].Subsets[0].Addresses = addrs
	s.Zone = big
	q := query("db.ns.svc.cluster.local.", TypeA, 0)
	if m, _ := Parse(s.Handle(q, maxUDP)); !m.Truncated || len(m.Answers) != 0 {
		t.Fatalf("UDP answer must be truncated: %+v", m)
	}
	if m, _ := Parse(s.Handle(q, 0)); m.Truncated || len(m.Answers) != 40 {
		t.Fatalf("TCP answer is complete: %d answers", len(m.Answers))
	}
	if m, _ := Parse(s.Handle(query("db.ns.svc.cluster.local.", TypeA, 4096), maxUDP)); m.Truncated || len(m.Answers) != 40 {
		t.Fatal("EDNS raises the UDP limit")
	}
}

func TestParseCompressedNames(t *testing.T) {
	// A reply from a real resolver: the answer's name and the CNAME target
	// point back into the question.
	q := query("www.example.com.", TypeA, 0)
	b := append([]byte(nil), q...)
	b[2] |= 0x80                                                     // response
	b[7] = 2                                                         // two answers
	b = append(b, 0xc0, 12, 0, 5, 0, 1, 0, 0, 0, 60, 0, 2, 0xc0, 16) // www.example.com CNAME example.com
	b = append(b, 0xc0, 16, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 93, 184, 215, 14)
	m, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(strs(m.Answers), "|"); got != "www.example.com. 60 CNAME example.com.|example.com. 60 A 93.184.215.14" {
		t.Fatalf("%s", got)
	}
	// Records survive repacking into another message.
	again, err := Parse((&Message{Answers: m.Answers}).Pack())
	if err != nil || strings.Join(strs(again.Answers), "|") != strings.Join(strs(m.Answers), "|") {
		t.Fatalf("repack: %v %v", err, strs(again.Answers))
	}
	if _, err := Parse(b[:20]); err == nil {
		t.Fatal("truncated messages are rejected")
	}
}

// A hostile packet: a long chain of labels and thousands of questions that
// all point at it. It must neither expand into huge names nor be echoed.
func TestHostileQueriesStaySmall(t *testing.T) {
	b := make([]byte, 12)
	b[5] = 0xff // QDCOUNT 255
	chain := len(b)
	for len(b) < 30000 {
		b = append(b, 63)
		b = append(b, strings.Repeat("a", 63)...)
	}
	b = append(b, 0, 0, 1, 0, 1)
	for range 255 {
		b = append(b, 0xc0|byte(chain>>8), byte(chain), 0, 1, 0, 1)
	}
	s := &Server{Zone: cluster(), Forward: func([]byte) ([]byte, error) { t.Fatal("must not forward"); return nil, nil }}
	reply := s.Handle(b, maxUDP)
	m, err := Parse(reply)
	if err != nil || m.Rcode != RcodeFormatError || len(m.Questions) != 0 || len(reply) != 12 {
		t.Fatalf("got %d bytes, %+v, %v", len(reply), m, err)
	}
	// The same chain in a single question is rejected as too long a name.
	b[5] = 1
	if _, err := Parse(b[:len(b)-255*6]); err == nil {
		t.Fatal("names longer than 255 bytes are invalid")
	}
	// Reserved label types (0x40-0xbf) are rejected.
	bad := append(make([]byte, 12), 0x41, 'a', 0, 0, 1, 0, 1)
	bad[5] = 1
	if _, err := Parse(bad); err == nil {
		t.Fatal("label length 65 is invalid")
	}
}
