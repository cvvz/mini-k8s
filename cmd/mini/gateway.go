package main

import (
	"context"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/gateway"
)

// runGateway serves the cluster's HTTP gateway on :8080.
func runGateway(ctx context.Context, c *client.Client, _ string) {
	g := gateway.NewGateway(c)
	go g.Run(ctx)
	serve(ctx, ":8080", g)
}
