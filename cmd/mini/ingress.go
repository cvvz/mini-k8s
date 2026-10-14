package main

import (
	"context"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/ingress"
)

// runIngressController serves the Ingresses on MINI_INGRESS_ADDR (":8081"
// by default). It runs as a hostNetwork static Pod on the control-plane
// node, published on 127.0.0.1:18080, and reaches the endpoints through the
// node's routes to every Pod subnet.
func runIngressController(ctx context.Context, c *client.Client, _ string) {
	ctrl := ingress.NewController(c, env("MINI_INGRESS_STATUS_HOSTNAME", "localhost"))
	go ctrl.Run(ctx)
	serve(ctx, env("MINI_INGRESS_ADDR", ":8081"), ctrl)
}
