package iptables

import (
	"errors"
	"strings"
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/intstr"
)

func svc(name, ip string, mutate func(*corev1.Service)) *corev1.Service {
	s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1"},
		Spec: corev1.ServiceSpec{ClusterIP: ip, Ports: []corev1.ServicePort{{Name: "p80", Port: 80, TargetPort: intstr.FromInt(80)}}}}
	if mutate != nil {
		mutate(s)
	}
	corev1.SetDefaultsServiceSpec(&s.Spec)
	return s
}

func eps(name string, port int32, ips ...string) *corev1.Endpoints {
	ss := corev1.EndpointSubset{Ports: []corev1.EndpointPort{{Name: "p80", Port: port, Protocol: corev1.ProtocolTCP}}}
	for _, ip := range ips {
		ss.Addresses = append(ss.Addresses, corev1.EndpointAddress{IP: ip})
	}
	return &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1"}, Subsets: []corev1.EndpointSubset{ss}}
}

// Chain names match the real kube-proxy's (the values appear in its tests).
func TestChainNamesMatchKubeProxy(t *testing.T) {
	if got := ServiceChain("ns1/svc1:p80", "TCP"); got != "KUBE-SVC-XPGD46QRK7WJZT7O" {
		t.Errorf("ServiceChain = %s", got)
	}
	if got := EndpointChain("ns1/svc1:p80", "TCP", "10.180.0.1:80"); got != "KUBE-SEP-SXIVWICOYRO3J4NJ" {
		t.Errorf("EndpointChain = %s", got)
	}
}

func TestBuildRules(t *testing.T) {
	services := []*corev1.Service{
		svc("svc1", "172.30.0.41", nil),
		svc("np", "172.30.0.42", func(s *corev1.Service) {
			s.Spec.Type = corev1.ServiceTypeNodePort
			s.Spec.Ports[0].NodePort = 30080
		}),
		svc("empty", "172.30.0.43", func(s *corev1.Service) {
			s.Spec.Type = corev1.ServiceTypeNodePort
			s.Spec.Ports[0].NodePort = 30081
		}),
		svc("sticky", "172.30.0.44", func(s *corev1.Service) { s.Spec.SessionAffinity = corev1.ServiceAffinityClientIP }),
		svc("legacy", "", nil),
	}
	endpoints := []*corev1.Endpoints{
		eps("svc1", 80, "10.180.0.1", "10.180.0.2", "10.180.0.3"),
		eps("np", 8080, "10.180.0.4"),
		eps("sticky", 80, "10.180.0.5"),
		eps("legacy", 80, "10.180.0.6"),
	}
	stale := "KUBE-SVC-AAAAAAAAAAAAAAAA"
	rules := string(BuildRules(services, endpoints, "10.244.0.0/16", []string{"KUBE-SERVICES", stale, "KUBE-SVC-XPGD46QRK7WJZT7O"}))
	svc1 := ServiceChain("ns1/svc1:p80", "TCP")
	sep1 := EndpointChain("ns1/svc1:p80", "TCP", "10.180.0.1:80")
	np := ServiceChain("ns1/np:p80", "TCP")
	ext := ExternalChain("ns1/np:p80", "TCP")
	sticky := EndpointChain("ns1/sticky:p80", "TCP", "10.180.0.5:80")
	for _, want := range []string{
		"*filter\n:KUBE-SERVICES - [0:0]\n",
		`-A KUBE-SERVICES -m comment --comment "ns1/empty:p80 has no endpoints" -d 172.30.0.43/32 -p tcp -m tcp --dport 80 -j REJECT --reject-with icmp-port-unreachable`,
		`-A KUBE-EXTERNAL-SERVICES -m comment --comment "ns1/empty:p80 has no endpoints" -m addrtype --dst-type LOCAL -p tcp -m tcp --dport 30081 -j REJECT`,
		"*nat\n",
		`-A KUBE-SERVICES -m comment --comment "ns1/svc1:p80 cluster IP" -d 172.30.0.41/32 -p tcp -m tcp --dport 80 -j ` + svc1,
		`-A ` + svc1 + ` -m comment --comment "ns1/svc1:p80 cluster IP" ! -s 10.244.0.0/16 -d 172.30.0.41/32 -p tcp -m tcp --dport 80 -j KUBE-MARK-MASQ`,
		`-A ` + svc1 + ` -m comment --comment "ns1/svc1:p80 -> 10.180.0.1:80" -m statistic --mode random --probability 0.3333333333 -j ` + sep1,
		`--comment "ns1/svc1:p80 -> 10.180.0.2:80" -m statistic --mode random --probability 0.5000000000 -j`,
		`-A ` + sep1 + ` -m comment --comment ns1/svc1:p80 -s 10.180.0.1/32 -j KUBE-MARK-MASQ`,
		`-A ` + sep1 + ` -m comment --comment ns1/svc1:p80 -m tcp -p tcp -j DNAT --to-destination 10.180.0.1:80`,
		`-A KUBE-NODEPORTS -m comment --comment ns1/np:p80 -p tcp -m tcp --dport 30080 -j ` + ext,
		`-A ` + ext + ` -m comment --comment "masquerade traffic for ns1/np:p80 external destinations" -j KUBE-MARK-MASQ`,
		`-A ` + ext + ` -j ` + np,
		`-m recent --name ` + sticky + ` --mask 255.255.255.255 --rsource --rcheck --seconds 10800 --reap -j ` + sticky,
		`-A ` + sticky + ` -m comment --comment ns1/sticky:p80 -m recent --name ` + sticky + ` --mask 255.255.255.255 --rsource --set -m tcp -p tcp -j DNAT --to-destination 10.180.0.5:80`,
		`-A KUBE-POSTROUTING -m comment --comment "kubernetes service traffic requiring SNAT" -j MASQUERADE --random-fully`,
		":" + stale + " - [0:0]\n",
		"-X " + stale + "\nCOMMIT\n",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("missing %q in\n%s", want, rules)
		}
	}
	// The last endpoint is taken unconditionally; the node port jump comes last.
	if !strings.Contains(rules, `--comment "ns1/svc1:p80 -> 10.180.0.3:80" -j KUBE-SEP-`) {
		t.Errorf("last endpoint must not have a probability:\n%s", rules)
	}
	nat := rules[strings.Index(rules, "*nat"):]
	var lastServicesRule string
	for _, line := range strings.Split(nat, "\n") {
		if strings.HasPrefix(line, "-A KUBE-SERVICES ") {
			lastServicesRule = line
		}
	}
	if !strings.Contains(lastServicesRule, "-j KUBE-NODEPORTS") {
		t.Errorf("KUBE-NODEPORTS must be the last rule of KUBE-SERVICES, got %q", lastServicesRule)
	}
	if strings.Contains(rules, "10.180.0.6") || strings.Contains(rules, "-X KUBE-SVC-XPGD46QRK7WJZT7O") {
		t.Errorf("Services without a cluster IP get no rules; active chains are kept:\n%s", rules)
	}
}

func TestSyncRestoresAndHooksChains(t *testing.T) {
	var calls []string
	var restored []byte
	installed := map[string]bool{}
	p := &Proxier{
		ClusterCIDR: "10.244.0.0/16",
		Exec: func(name string, args ...string) (string, error) {
			cmd := name + " " + strings.Join(args, " ")
			calls = append(calls, cmd)
			switch {
			case name == "iptables-save":
				return "*nat\n:PREROUTING ACCEPT [0:0]\n:KUBE-SEP-OLDOLDOLDOLDOLD - [0:0]\nCOMMIT\n", nil
			case strings.Contains(cmd, " -C "):
				if !installed[strings.Replace(cmd, " -C ", " ", 1)] {
					return "", errors.New("no such rule")
				}
			case strings.Contains(cmd, " -I "):
				installed[strings.Replace(cmd, " -I ", " ", 1)] = true
			}
			return "", nil
		},
		Restore: func(data []byte) error { restored = data; return nil },
	}
	services := []*corev1.Service{svc("svc1", "172.30.0.41", nil)}
	endpoints := []*corev1.Endpoints{eps("svc1", 80, "10.180.0.1")}
	if err := p.sync(services, endpoints, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "-X KUBE-SEP-OLDOLDOLDOLDOLD") {
		t.Errorf("stale chain not removed:\n%s", restored)
	}
	if len(installed) != len(jumps) {
		t.Fatalf("installed %d jumps, want %d: %v", len(installed), len(jumps), calls)
	}
	restored, calls = nil, nil
	if err := p.sync(services, endpoints, false); err != nil || restored != nil {
		t.Fatalf("an unchanged sync must not rewrite rules: %v %s", err, restored)
	}
	if err := p.sync(services, endpoints, true); err != nil || restored == nil {
		t.Fatalf("a forced sync rewrites the rules: %v", err)
	}
	for _, c := range calls {
		if strings.Contains(c, " -I ") {
			t.Errorf("jump inserted twice: %s", c)
		}
	}
}
