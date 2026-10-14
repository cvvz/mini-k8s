package main

import (
	"context"
	"log"
	"os"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/scheduler"
)

// runScheduler binds pending Pods to nodes.
func runScheduler(ctx context.Context, c *client.Client, _ string) {
	host, _ := os.Hostname()
	rec := record.New(c, "default-scheduler", host)
	pods := informers.New(client.Pods(c, ""), client.ListOptions{}, 0)
	nodes := informers.New(client.Nodes(c), client.ListOptions{}, 0)
	s := scheduler.New(c, pods, nodes, rec)
	go rec.Run(ctx)
	go pods.Run(ctx)
	go nodes.Run(ctx)
	informers.WaitForSync(ctx, pods.HasSynced, nodes.HasSynced)
	log.Print("kube-scheduler started")
	s.Run(ctx)
}
