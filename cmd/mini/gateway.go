package main

import (
	"context"
	"log"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/controller/nodeipam"
	"mini-k8s/pkg/gateway"
	"mini-k8s/pkg/network"
	"mini-k8s/pkg/network/hostgw"
)

// runGateway serves the cluster's HTTP gateway on :8080. The gateway is not
// a node, so it keeps host-gw routes to every node's Pod subnet to reach the
// backends.
func runGateway(ctx context.Context, c *client.Client, _ string) {
	go keepRoutes(ctx, c, &hostgw.Agent{Run: network.Exec,
		ClusterCIDR: env("MINI_CLUSTER_CIDR", nodeipam.DefaultClusterCIDR)})
	g := gateway.NewGateway(c)
	go g.Run(ctx)
	serve(ctx, ":8080", g)
}

// keepRoutes syncs agent on every Node change and every 10 seconds.
func keepRoutes(ctx context.Context, c *client.Client, agent *hostgw.Agent) {
	changed := make(chan struct{}, 1)
	poke := func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	nodes := informers.New(client.Nodes(c), client.ListOptions{}, 0)
	nodes.AddHandler(informers.Handler[*corev1.Node]{
		OnAdd: func(*corev1.Node) { poke() }, OnUpdate: func(_, _ *corev1.Node) { poke() }, OnDelete: func(*corev1.Node) { poke() },
	})
	go nodes.Run(ctx)
	if !informers.WaitForSync(ctx, nodes.HasSynced) {
		return
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if err := agent.Sync(nodes.List("", nil)); err != nil {
			log.Printf("gateway: sync routes: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-ticker.C:
		}
	}
}
