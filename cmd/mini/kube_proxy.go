package main

import (
	"context"
	"log"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/controller/nodeipam"
	"mini-k8s/pkg/proxy/iptables"
)

// runKubeProxy programs the node's iptables for Services. It runs as a
// privileged hostNetwork static Pod on every node (a DaemonSet once those
// exist), from an image that ships iptables.
func runKubeProxy(ctx context.Context, c *client.Client, _ string) {
	log.Print("kube-proxy started (iptables mode)")
	iptables.NewProxier(c, env("MINI_CLUSTER_CIDR", nodeipam.DefaultClusterCIDR)).Run(ctx)
}
