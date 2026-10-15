package main

import (
	"context"
	"log"
	"strconv"
	"time"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/metricsserver"
)

// runMetricsServer scrapes the kubelets every MINI_METRIC_RESOLUTION (15s
// by default, like metrics-server's --metric-resolution) and serves the
// metrics.k8s.io API on :8080, which the API server proxies to. It runs
// as the metrics-server Deployment in kube-system.
func runMetricsServer(ctx context.Context, c *client.Client, token string) {
	resolution, err := time.ParseDuration(env("MINI_METRIC_RESOLUTION", "15s"))
	if err != nil || resolution < time.Second {
		log.Fatalf("invalid MINI_METRIC_RESOLUTION %q", env("MINI_METRIC_RESOLUTION", ""))
	}
	port, err := strconv.Atoi(env("MINI_KUBELET_PORT", "10250"))
	if err != nil {
		log.Fatal(err)
	}
	s := metricsserver.New(c, token, port, resolution)
	go s.Run(ctx)
	serve(ctx, ":8080", s.Handler())
}
