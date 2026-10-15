package cni

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
)

type recorder struct {
	mu    sync.Mutex
	calls []string
	fail  []string // command prefixes that fail
}

func (r *recorder) run(name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cmd := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, cmd)
	for _, f := range r.fail {
		if strings.HasPrefix(cmd, f) {
			return "", errors.New("boom")
		}
	}
	return "", nil
}

func (r *recorder) has(cmd string) bool {
	for _, c := range r.calls {
		if c == cmd {
			return true
		}
	}
	return false
}

const conf = `{"cniVersion":"1.0.0","name":"mini","type":"mini-cni","bridge":"cni0","subnet":"10.244.1.0/24"}`

func TestIPAMAllocatesLowestFreeAndIsIdempotent(t *testing.T) {
	s := IPAM{Dir: t.TempDir()}
	_, subnet, _ := net.ParseCIDR("10.244.1.0/24")
	a, _ := s.Allocate("mini", "c1", subnet)
	b, _ := s.Allocate("mini", "c2", subnet)
	again, _ := s.Allocate("mini", "c1", subnet)
	if a.String() != "10.244.1.2" || b.String() != "10.244.1.3" || !again.Equal(a) {
		t.Fatalf("a=%s b=%s again=%s", a, b, again)
	}
	if err := s.Release("mini", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup("mini", "c1"); ok {
		t.Fatal("released address still allocated")
	}
	if c, _ := s.Allocate("mini", "c3", subnet); c.String() != "10.244.1.2" {
		t.Fatalf("released address not reused: %s", c)
	}
	if err := s.Release("mini", "unknown"); err != nil {
		t.Fatalf("releasing an unknown container must be a no-op: %v", err)
	}
}

func TestIPAMExhaustion(t *testing.T) {
	s := IPAM{Dir: t.TempDir()}
	_, subnet, _ := net.ParseCIDR("10.0.0.0/29") // hosts .2 to .6 after the gateway
	for i := 0; i < 5; i++ {
		if _, err := s.Allocate("n", string(rune('a'+i)), subnet); err != nil {
			t.Fatalf("allocation %d: %v", i, err)
		}
	}
	if _, err := s.Allocate("n", "z", subnet); err == nil {
		t.Fatal("full subnet must fail")
	}
}

func TestAddWiresVethAndReturnsResult(t *testing.T) {
	r := &recorder{fail: []string{"ip link show cni0"}} // the bridge does not exist yet
	p := Plugin{Run: r.run, IPAM: IPAM{Dir: t.TempDir()}}
	out, err := p.Exec(Args{Command: "ADD", ContainerID: "0123456789abcdef", Netns: "/var/run/netns/cni-1", IfName: "eth0"}, []byte(conf))
	if err != nil {
		t.Fatal(err)
	}
	var res Result
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.CNIVersion != "1.0.0" || res.IPs[0].Address != "10.244.1.2/24" || res.IPs[0].Gateway != "10.244.1.1" ||
		res.Interfaces[res.IPs[0].Interface].Sandbox != "/var/run/netns/cni-1" {
		t.Fatalf("result: %s", out)
	}
	for _, want := range []string{
		"ip link add cni0 type bridge",
		"ip addr replace 10.244.1.1/24 dev cni0",
		"ip link add veth0123456789a type veth peer name eth0 netns cni-1",
		"ip link set veth0123456789a master cni0 up",
		"ip link set veth0123456789a type bridge_slave hairpin on",
		"ip -n cni-1 addr add 10.244.1.2/24 dev eth0",
		"ip neigh flush to 10.244.1.2 dev cni0",
		"ip netns exec cni-1 sysctl -q -w net.ipv4.conf.eth0.arp_notify=1",
		"ip -n cni-1 link set lo up",
		"ip -n cni-1 route add default via 10.244.1.1",
	} {
		if !r.has(want) {
			t.Errorf("missing %q in %v", want, r.calls)
		}
	}
}

func TestLoopbackBringsUpLo(t *testing.T) {
	r := &recorder{}
	p := Plugin{Run: r.run, IPAM: IPAM{Dir: t.TempDir()}}
	lo := []byte(`{"cniVersion":"1.0.0","name":"cni-loopback","type":"loopback"}`)
	out, err := p.Exec(Args{Command: "ADD", ContainerID: "c1", Netns: "/var/run/netns/cni-1", IfName: "eth0"}, lo)
	if err != nil {
		t.Fatal(err)
	}
	var res Result
	if err := json.Unmarshal(out, &res); err != nil || res.IPs[0].Address != "127.0.0.1/8" || res.Interfaces[0].Name != "lo" {
		t.Fatalf("result %s: %v", out, err)
	}
	if len(r.calls) != 1 || !r.has("ip -n cni-1 link set lo up") {
		t.Fatalf("calls: %v", r.calls)
	}
	for _, cmd := range []string{"DEL", "CHECK"} {
		if _, err := p.Exec(Args{Command: cmd, ContainerID: "c1"}, lo); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
	}
	if len(r.calls) != 1 {
		t.Fatalf("DEL/CHECK must not touch the network: %v", r.calls)
	}
}

func TestAddRollsBackOnFailureAndDelIsIdempotent(t *testing.T) {
	r := &recorder{fail: []string{"ip -n cni-1 addr add"}}
	p := Plugin{Run: r.run, IPAM: IPAM{Dir: t.TempDir()}}
	args := Args{Command: "ADD", ContainerID: "abc", Netns: "/var/run/netns/cni-1", IfName: "eth0"}
	if _, err := p.Exec(args, []byte(conf)); err == nil {
		t.Fatal("expected failure")
	}
	if _, ok := p.IPAM.Lookup("mini", "abc"); ok {
		t.Fatal("failed ADD must release its address")
	}
	if !r.has("ip link del vethabc") {
		t.Fatal("failed ADD must delete its veth")
	}
	args.Command = "DEL"
	for i := 0; i < 2; i++ {
		if _, err := p.Exec(args, []byte(conf)); err != nil {
			t.Fatalf("DEL %d: %v", i, err)
		}
	}
}

func TestMainReportsVersionAndErrors(t *testing.T) {
	var out bytes.Buffer
	env := map[string]string{"CNI_COMMAND": "VERSION"}
	if code := Main(strings.NewReader(conf), &out, func(k string) string { return env[k] }); code != 0 || !strings.Contains(out.String(), `"1.0.0"`) {
		t.Fatalf("VERSION: %d %s", code, out.String())
	}
	out.Reset()
	env = map[string]string{"CNI_COMMAND": "ADD"} // no container ID
	if code := Main(strings.NewReader(conf), &out, func(k string) string { return env[k] }); code != 1 || !strings.Contains(out.String(), `"code":100`) {
		t.Fatalf("error result: %d %s", code, out.String())
	}
}
