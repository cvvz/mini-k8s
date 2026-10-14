package hostgw

import (
	"errors"
	"strings"
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

func node(name, cidr, ip string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{PodCIDR: cidr}}
	if ip != "" {
		n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}
	}
	return n
}

type fakeHost struct {
	routes   string
	masq     bool
	commands []string
}

func (h *fakeHost) run(name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	switch {
	case cmd == "ip -j route show":
		return h.routes, nil
	case strings.HasPrefix(cmd, "iptables -t nat -C"):
		if !h.masq {
			return "", errors.New("rule missing")
		}
		return "", nil
	}
	h.commands = append(h.commands, cmd)
	return "", nil
}

func TestDesiredRoutesSkipSelfAndIncompleteNodes(t *testing.T) {
	got := DesiredRoutes([]*corev1.Node{
		node("worker-1", "10.244.1.0/24", "172.19.0.2"),
		node("worker-2", "10.244.2.0/24", "172.19.0.3"),
		node("worker-3", "", "172.19.0.4"),
		node("worker-4", "10.244.4.0/24", ""),
	}, "worker-1")
	if len(got) != 1 || got["10.244.2.0/24"] != "172.19.0.3" {
		t.Fatalf("routes: %v", got)
	}
}

func TestSyncAddsReplacesAndRemovesOnlyOwnRoutes(t *testing.T) {
	h := &fakeHost{routes: `[
		{"dst":"default","gateway":"172.19.0.1"},
		{"dst":"10.244.1.0/24","dev":"cni0"},
		{"dst":"10.244.2.0/24","gateway":"172.19.0.9"},
		{"dst":"10.244.7.0/24","gateway":"172.19.0.7"},
		{"dst":"192.168.0.0/24","gateway":"172.19.0.1"}]`}
	a := &Agent{Run: h.run, Self: "worker-1", ClusterCIDR: "10.244.0.0/16", Masquerade: true}
	nodes := []*corev1.Node{
		node("worker-1", "10.244.1.0/24", "172.19.0.2"),
		node("worker-2", "10.244.2.0/24", "172.19.0.3"),
		node("worker-3", "10.244.3.0/24", "172.19.0.4"),
	}
	if err := a.Sync(nodes); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ip route replace 10.244.2.0/24 via 172.19.0.3",
		"ip route replace 10.244.3.0/24 via 172.19.0.4",
		"ip route del 10.244.7.0/24",
		"iptables -t nat -A POSTROUTING -s 10.244.0.0/16 ! -d 10.244.0.0/16 -m comment --comment mini-k8s pod egress -j MASQUERADE",
	}
	if strings.Join(h.commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(h.commands, "\n"), strings.Join(want, "\n"))
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	h := &fakeHost{masq: true, routes: `[{"dst":"10.244.2.0/24","gateway":"172.19.0.3"}]`}
	a := &Agent{Run: h.run, Self: "worker-1", ClusterCIDR: "10.244.0.0/16", Masquerade: true}
	if err := a.Sync([]*corev1.Node{node("worker-2", "10.244.2.0/24", "172.19.0.3")}); err != nil {
		t.Fatal(err)
	}
	if len(h.commands) != 0 {
		t.Fatalf("steady state must not change anything: %v", h.commands)
	}
}
