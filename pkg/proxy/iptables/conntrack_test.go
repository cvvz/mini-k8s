package iptables

import (
	"errors"
	"sort"
	"strings"
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

func TestStaleUDPFlowsAreDeleted(t *testing.T) {
	var deletes []string
	p := &Proxier{
		ClusterCIDR: "10.244.0.0/16",
		Exec: func(name string, args ...string) (string, error) {
			if name == "conntrack" {
				deletes = append(deletes, strings.Join(args, " "))
				return "", errors.New("conntrack v1.4.8 (conntrack-tools): 0 flow entries have been deleted")
			}
			return "", nil
		},
		Restore: func([]byte) error { return nil },
	}
	dns := svc("dns", "10.96.0.10", func(s *corev1.Service) {
		s.Spec.Type = corev1.ServiceTypeNodePort
		s.Spec.Ports = []corev1.ServicePort{{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, NodePort: 30053}}
	})
	web := svc("web", "10.96.0.20", nil) // TCP: never touched
	udpEps := func(ips ...string) *corev1.Endpoints {
		e := eps("dns", 53, ips...)
		e.Subsets[0].Ports = []corev1.EndpointPort{{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP}}
		return e
	}
	sync := func(endpoints ...*corev1.Endpoints) []string {
		deletes = nil
		if err := p.sync([]*corev1.Service{dns, web}, append(endpoints, eps("web", 80, "10.244.9.9")), true); err != nil {
			t.Fatal(err)
		}
		sort.Strings(deletes)
		return deletes
	}

	if got := sync(&corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "dns", Namespace: "ns1"}}); len(got) != 0 {
		t.Fatalf("the first sync deletes nothing: %v", got)
	}
	got := sync(udpEps("10.244.1.5", "10.244.2.5"))
	want := []string{"-D -p udp --orig-dst 10.96.0.10", "-D -p udp --orig-port-dst 30053"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("gaining endpoints clears the rejected flows: got %v, want %v", got, want)
	}
	got = sync(udpEps("10.244.2.5"))
	want = []string{"-D -p udp --orig-dst 10.96.0.10 --dst-nat 10.244.1.5", "-D -p udp --orig-port-dst 30053 --dst-nat 10.244.1.5"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("a removed endpoint loses its flows: got %v, want %v", got, want)
	}
	if got := sync(udpEps("10.244.2.5")); len(got) != 0 {
		t.Fatalf("nothing changed: %v", got)
	}
}
