package main

import (
	"context"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/gateway"
)

// runGateway serves the cluster's HTTP gateway on MINI_GATEWAY_ADDR (":8080"
// by default). It runs as a hostNetwork static Pod on the control-plane node,
// whose kubelet keeps the routes to every node's Pod subnet.
func runGateway(ctx context.Context, c *client.Client, _ string) {
	g := gateway.NewGateway(c)
	go g.Run(ctx)
	serve(ctx, env("MINI_GATEWAY_ADDR", ":8080"), g)
}
