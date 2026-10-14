// Package cni implements mini-cni, the Pod network plugin containerd calls
// for every sandbox (CNI 1.0: ADD, DEL, CHECK, VERSION). It connects the
// Pod's network namespace to the node bridge with a veth pair and assigns an
// address from the node's Pod CIDR; host-gw routes (package hostgw) connect
// the bridges of different nodes. It also serves as the "loopback" plugin
// containerd invokes to bring up lo in every sandbox.
package cni

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"mini-k8s/pkg/network"
)

// Version is the CNI specification version this plugin speaks.
const Version = "1.0.0"

// DefaultIPAMDir holds address allocations (tmpfs in a node container).
const DefaultIPAMDir = "/run/mini-cni/ipam"

// NetConf is the plugin's network configuration (one entry of a conflist).
type NetConf struct {
	CNIVersion string `json:"cniVersion"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	// Bridge is the node bridge, e.g. cni0.
	Bridge string `json:"bridge"`
	// Subnet is the node's Pod CIDR; its first host address is the gateway.
	Subnet string `json:"subnet"`
}

// Args are the CNI_* parameters of one invocation.
type Args struct {
	Command     string
	ContainerID string
	Netns       string
	IfName      string
}

// Result is the CNI 1.0 success result.
type Result struct {
	CNIVersion string      `json:"cniVersion"`
	Interfaces []Interface `json:"interfaces"`
	IPs        []IPConfig  `json:"ips"`
	Routes     []Route     `json:"routes,omitempty"`
}

// Interface is one interface touched by the plugin.
type Interface struct {
	Name    string `json:"name"`
	Sandbox string `json:"sandbox,omitempty"`
}

// IPConfig is an assigned address; Interface indexes Result.Interfaces.
type IPConfig struct {
	Address   string `json:"address"`
	Gateway   string `json:"gateway,omitempty"`
	Interface int    `json:"interface"`
}

// Route is a route installed in the sandbox.
type Route struct {
	Dst string `json:"dst"`
	GW  string `json:"gw,omitempty"`
}

// Error is the CNI error result.
type Error struct {
	CNIVersion string `json:"cniVersion"`
	Code       int    `json:"code"`
	Msg        string `json:"msg"`
}

// Plugin executes CNI commands.
type Plugin struct {
	Run  network.Runner
	IPAM IPAM
}

// Main runs the plugin as a CNI executable and returns the process exit code.
func Main(stdin io.Reader, stdout io.Writer, getenv func(string) string) int {
	p := Plugin{Run: network.Exec, IPAM: IPAM{Dir: DefaultIPAMDir}}
	args := Args{Command: getenv("CNI_COMMAND"), ContainerID: getenv("CNI_CONTAINERID"),
		Netns: getenv("CNI_NETNS"), IfName: getenv("CNI_IFNAME")}
	conf, err := io.ReadAll(stdin)
	if err == nil {
		var out []byte
		if out, err = p.Exec(args, conf); err == nil {
			_, _ = stdout.Write(out)
			return 0
		}
	}
	data, _ := json.Marshal(Error{CNIVersion: Version, Code: 100, Msg: err.Error()})
	_, _ = stdout.Write(data)
	return 1
}

// Exec runs one CNI command and returns its JSON output.
func (p Plugin) Exec(a Args, confData []byte) ([]byte, error) {
	if a.Command == "VERSION" {
		return json.Marshal(map[string]any{"cniVersion": Version, "supportedVersions": []string{"0.4.0", "1.0.0"}})
	}
	var conf NetConf
	if err := json.Unmarshal(confData, &conf); err != nil {
		return nil, fmt.Errorf("parse network configuration: %w", err)
	}
	if conf.Type == "loopback" {
		return p.loopback(a)
	}
	if conf.Bridge == "" || conf.Subnet == "" || conf.Name == "" {
		return nil, fmt.Errorf("network configuration needs name, bridge and subnet")
	}
	if a.ContainerID == "" {
		return nil, fmt.Errorf("CNI_CONTAINERID is required")
	}
	switch a.Command {
	case "ADD":
		res, err := p.add(conf, a)
		if err != nil {
			return nil, err
		}
		return json.Marshal(res)
	case "DEL":
		return nil, p.del(conf, a)
	case "CHECK":
		if _, ok := p.IPAM.Lookup(conf.Name, a.ContainerID); !ok {
			return nil, fmt.Errorf("container %s has no address in %s", a.ContainerID, conf.Name)
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unsupported CNI_COMMAND %q", a.Command)
}

// loopback brings up lo in the sandbox. DEL and CHECK have nothing to do:
// lo disappears with the namespace.
func (p Plugin) loopback(a Args) ([]byte, error) {
	if a.Command != "ADD" {
		return nil, nil
	}
	if a.Netns == "" {
		return nil, fmt.Errorf("CNI_NETNS is required for ADD")
	}
	ns, cleanup, err := netnsName(a.Netns, a.ContainerID)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if _, err := p.Run("ip", "-n", ns, "link", "set", "lo", "up"); err != nil {
		return nil, err
	}
	return json.Marshal(Result{CNIVersion: Version, Interfaces: []Interface{{Name: "lo", Sandbox: a.Netns}},
		IPs: []IPConfig{{Address: "127.0.0.1/8", Interface: 0}}})
}

// HostVeth names the node-side end of a container's veth pair.
func HostVeth(containerID string) string {
	if len(containerID) > 11 {
		containerID = containerID[:11]
	}
	return "veth" + containerID
}

func (p Plugin) add(conf NetConf, a Args) (*Result, error) {
	gw, subnet, err := gateway(conf.Subnet)
	if err != nil {
		return nil, err
	}
	if a.Netns == "" || a.IfName == "" {
		return nil, fmt.Errorf("CNI_NETNS and CNI_IFNAME are required for ADD")
	}
	ns, cleanup, err := netnsName(a.Netns, a.ContainerID)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	ones, _ := subnet.Mask.Size()
	if err := p.ensureBridge(conf.Bridge, fmt.Sprintf("%s/%d", gw, ones)); err != nil {
		return nil, err
	}
	ip, err := p.IPAM.Allocate(conf.Name, a.ContainerID, subnet)
	if err != nil {
		return nil, err
	}
	host := HostVeth(a.ContainerID)
	addr := fmt.Sprintf("%s/%d", ip, ones)
	steps := [][]string{
		{"link", "add", host, "type", "veth", "peer", "name", a.IfName, "netns", ns},
		{"link", "set", host, "master", conf.Bridge, "up"},
		// Hairpin lets a Pod reach itself through its Service's cluster IP:
		// the DNATed packet leaves through the port it arrived on.
		{"link", "set", host, "type", "bridge_slave", "hairpin", "on"},
		{"-n", ns, "addr", "add", addr, "dev", a.IfName},
		{"-n", ns, "link", "set", a.IfName, "up"},
		{"-n", ns, "link", "set", "lo", "up"},
		{"-n", ns, "route", "add", "default", "via", gw.String()},
	}
	_, _ = p.Run("ip", "link", "del", host) // a leftover from an interrupted ADD
	for _, step := range steps {
		if _, err := p.Run("ip", step...); err != nil {
			_, _ = p.Run("ip", "link", "del", host)
			_ = p.IPAM.Release(conf.Name, a.ContainerID)
			return nil, err
		}
	}
	return &Result{
		CNIVersion: Version,
		Interfaces: []Interface{{Name: conf.Bridge}, {Name: host}, {Name: a.IfName, Sandbox: a.Netns}},
		IPs:        []IPConfig{{Address: addr, Gateway: gw.String(), Interface: 2}},
		Routes:     []Route{{Dst: "0.0.0.0/0", GW: gw.String()}},
	}, nil
}

func (p Plugin) del(conf NetConf, a Args) error {
	// Deleting the namespace removes the veth pair; this covers a namespace
	// that outlived the sandbox. Both steps are idempotent.
	_, _ = p.Run("ip", "link", "del", HostVeth(a.ContainerID))
	return p.IPAM.Release(conf.Name, a.ContainerID)
}

func (p Plugin) ensureBridge(name, addr string) error {
	if _, err := p.Run("ip", "link", "show", name); err != nil {
		if _, err := p.Run("ip", "link", "add", name, "type", "bridge"); err != nil && !strings.Contains(err.Error(), "File exists") {
			return err
		}
	}
	if _, err := p.Run("ip", "addr", "replace", addr, "dev", name); err != nil {
		return err
	}
	_, err := p.Run("ip", "link", "set", name, "up")
	return err
}

// gateway returns the subnet's first host address and the subnet.
func gateway(cidr string) (net.IP, *net.IPNet, error) {
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil || subnet.IP.To4() == nil {
		return nil, nil, fmt.Errorf("invalid IPv4 subnet %q", cidr)
	}
	return uintToIP(ipToUint(subnet.IP) + 1), subnet, nil
}

// netnsName returns a name usable with `ip -n`/`ip link set netns`. containerd
// pins namespaces under /var/run/netns; other paths get a temporary link.
func netnsName(path, containerID string) (string, func(), error) {
	dir := filepath.Dir(path)
	if dir == "/var/run/netns" || dir == "/run/netns" {
		return filepath.Base(path), func() {}, nil
	}
	name := "mini-" + HostVeth(containerID)
	if err := os.MkdirAll("/run/netns", 0o755); err != nil {
		return "", nil, err
	}
	link := filepath.Join("/run/netns", name)
	_ = os.Remove(link)
	if err := os.Symlink(path, link); err != nil {
		return "", nil, err
	}
	return name, func() { _ = os.Remove(link) }, nil
}
