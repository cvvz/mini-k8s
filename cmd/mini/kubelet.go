package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/controller/nodeipam"
	"mini-k8s/pkg/kubelet"
	"mini-k8s/pkg/kubelet/dns"
	"mini-k8s/pkg/kubelet/kuberuntime"
	"mini-k8s/pkg/network"
	"mini-k8s/pkg/network/hostgw"
	"mini-k8s/pkg/nodeinit"
)

// runKubelet runs the node agent for MINI_NODE against the node's containerd
// and serves its API on :10250. It returns when ctx ends.
func runKubelet(ctx context.Context, c *client.Client, token string) {
	node := os.Getenv("MINI_NODE")
	if node == "" {
		log.Fatal("MINI_NODE is required")
	}
	conn, err := kuberuntime.Dial(nodeinit.DefaultContainerdSocket)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	rt := kuberuntime.NewManager(runtimeapi.NewRuntimeServiceClient(conn), runtimeapi.NewImageServiceClient(conn))
	address, err := nodeAddress()
	if err != nil {
		log.Fatal(err)
	}
	// Pods resolve names through a forwarder on the node address, which
	// relays to the node's own resolver (Docker's embedded DNS).
	upstream, err := dns.Upstream("/etc/resolv.conf")
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		if err := dns.Serve(ctx, net.JoinHostPort(address, "53"), upstream); err != nil {
			log.Printf("kubelet: dns forwarder: %v", err)
		}
	}()
	searches, err := dns.Searches("/etc/resolv.conf")
	if err != nil {
		log.Fatal(err)
	}
	domain := env("MINI_CLUSTER_DOMAIN", "cluster.local")
	dnsConfigurer := &dns.Configurer{ClusterDNS: []string{env("MINI_CLUSTER_DNS", "10.96.0.10")}, ClusterDomain: domain,
		HostServers: []string{address}, HostSearches: searches}
	rt.DNS, rt.ClusterDomain = dnsConfigurer.For, domain
	rec := record.New(c, "kubelet", node)
	k := kubelet.New(c, rt, rec, kubelet.Config{
		NodeName:     node,
		Address:      address,
		Token:        token,
		SandboxImage: env("MINI_SANDBOX_IMAGE", nodeinit.DefaultSandboxImage),
		CNIConfDir:   "/etc/cni/net.d",
		// Only nodes that have the directory run static Pods (the control plane).
		StaticPodPath: env("MINI_STATIC_PODS", kubelet.DefaultStaticPodPath),
		Role:          os.Getenv("MINI_NODE_ROLE"),
		Routes: &hostgw.Agent{Run: network.Exec, Self: node, Masquerade: true,
			ClusterCIDR: env("MINI_CLUSTER_CIDR", nodeipam.DefaultClusterCIDR)},
	})
	rt.Probes = k.Probes()
	rt.Env = k.ServiceEnv
	rt.Objects, rt.NodeIP, rt.Allocatable, rt.Recorder = k, address, kubelet.NodeCapacity("/proc/meminfo"), rec
	go rec.Run(ctx)
	go k.Run(ctx)
	serve(ctx, ":10250", k.Handler())
}

// nodeAddress returns this container's IPv4 address on the cluster network.
func nodeAddress() (string, error) {
	iface, err := net.InterfaceByName("eth0")
	if err != nil {
		return "", err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			return ipnet.IP.String(), nil
		}
	}
	return "", fmt.Errorf("eth0 has no IPv4 address")
}
