// Package hostgw connects the Pod subnets of all nodes, like flannel's
// host-gw backend: on every host it keeps one route per other node,
// "<that node's podCIDR> via <that node's InternalIP>", and masquerades Pod
// traffic that leaves the cluster CIDR. Nodes share one L2 network (the
// Docker network), so the next hop is always directly reachable.
package hostgw

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/network"
)

// Agent reconciles routes and the masquerade rule on one host.
type Agent struct {
	Run network.Runner
	// Self is the local node, whose own subnet is reached through its bridge;
	// empty on hosts that are not nodes.
	Self string
	// ClusterCIDR covers every Pod subnet, e.g. 10.244.0.0/16.
	ClusterCIDR string
	// Masquerade source-NATs Pod traffic to destinations outside ClusterCIDR.
	Masquerade bool
}

// DesiredRoutes maps every other node's Pod CIDR to its InternalIP.
func DesiredRoutes(nodes []*corev1.Node, self string) map[string]string {
	routes := map[string]string{}
	for _, n := range nodes {
		if n.Name == self || n.Spec.PodCIDR == "" || n.InternalIP() == "" {
			continue
		}
		routes[n.Spec.PodCIDR] = n.InternalIP()
	}
	return routes
}

type ipRoute struct {
	Dst     string `json:"dst"`
	Gateway string `json:"gateway"`
}

// currentRoutes returns gatewayed routes inside the cluster CIDR (ours).
func (a *Agent) currentRoutes(cluster *net.IPNet) (map[string]string, error) {
	out, err := a.Run("ip", "-j", "route", "show")
	if err != nil {
		return nil, err
	}
	var routes []ipRoute
	if err := json.Unmarshal([]byte(out), &routes); err != nil {
		return nil, fmt.Errorf("parse ip -j route: %w", err)
	}
	current := map[string]string{}
	for _, r := range routes {
		ip, dst, err := net.ParseCIDR(r.Dst)
		if err != nil || r.Gateway == "" || !cluster.Contains(ip) {
			continue
		}
		current[dst.String()] = r.Gateway
	}
	return current, nil
}

// Sync makes the host's routes match the node list and ensures masquerading.
func (a *Agent) Sync(nodes []*corev1.Node) error {
	_, cluster, err := net.ParseCIDR(a.ClusterCIDR)
	if err != nil {
		return err
	}
	current, err := a.currentRoutes(cluster)
	if err != nil {
		return err
	}
	desired := DesiredRoutes(nodes, a.Self)
	for _, dst := range sortedKeys(desired) {
		if current[dst] != desired[dst] {
			if _, err := a.Run("ip", "route", "replace", dst, "via", desired[dst]); err != nil {
				return err
			}
		}
	}
	for _, dst := range sortedKeys(current) {
		if _, ok := desired[dst]; !ok {
			if _, err := a.Run("ip", "route", "del", dst); err != nil {
				return err
			}
		}
	}
	if !a.Masquerade {
		return nil
	}
	rule := []string{"POSTROUTING", "-s", a.ClusterCIDR, "!", "-d", a.ClusterCIDR,
		"-m", "comment", "--comment", "mini-k8s pod egress", "-j", "MASQUERADE"}
	if _, err := a.Run("iptables", append([]string{"-t", "nat", "-C"}, rule...)...); err == nil {
		return nil
	}
	_, err = a.Run("iptables", append([]string{"-t", "nat", "-A"}, rule...)...)
	return err
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
