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
	volumes := informers.New(client.PersistentVolumes(c), client.ListOptions{}, 0)
	claims := informers.New(client.PersistentVolumeClaims(c, ""), client.ListOptions{}, 0)
	classes := informers.New(client.StorageClasses(c), client.ListOptions{}, 0)
	s := scheduler.New(c, pods, nodes, rec).WithVolumes(volumes, claims, classes)
	go rec.Run(ctx)
	for _, run := range []func(context.Context){pods.Run, nodes.Run, volumes.Run, claims.Run, classes.Run} {
		go run(ctx)
	}
	informers.WaitForSync(ctx, pods.HasSynced, nodes.HasSynced, volumes.HasSynced, claims.HasSynced, classes.HasSynced)
	log.Print("kube-scheduler started")
	s.Run(ctx)
}
