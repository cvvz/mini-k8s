package main

import (
	"context"
	"log"
	"os"
	"time"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/controller/deployment"
	"mini-k8s/pkg/controller/garbagecollector"
	"mini-k8s/pkg/controller/namespace"
)

// runControllerManager runs every controller against shared informers.
func runControllerManager(ctx context.Context, c *client.Client, _ string) {
	host, _ := os.Hostname()
	rec := record.New(c, "kube-controller-manager", host)
	pods := informers.New(client.Pods(c, ""), client.ListOptions{}, 30*time.Second)
	deployments := informers.New(client.Deployments(c, ""), client.ListOptions{}, 30*time.Second)
	namespaces := informers.New(client.Namespaces(c), client.ListOptions{}, 30*time.Second)
	dc := deployment.NewDeploymentController(c, deployments, pods, rec)
	nc := namespace.NewNamespaceController(c, namespaces)
	gc := garbagecollector.NewGarbageCollector(c, garbagecollector.TrackedResources())
	go rec.Run(ctx)
	for _, run := range []func(context.Context){pods.Run, deployments.Run, namespaces.Run} {
		go run(ctx)
	}
	informers.WaitForSync(ctx, pods.HasSynced, deployments.HasSynced, namespaces.HasSynced)
	go nc.Run(ctx, 2)
	go gc.Run(ctx)
	log.Print("kube-controller-manager started")
	dc.Run(ctx, 4)
}
