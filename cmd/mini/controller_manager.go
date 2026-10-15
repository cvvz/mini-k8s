package main

import (
	"context"
	"log"
	"os"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/controller/deployment"
	"mini-k8s/pkg/controller/endpoint"
	"mini-k8s/pkg/controller/garbagecollector"
	"mini-k8s/pkg/controller/namespace"
	"mini-k8s/pkg/controller/nodeipam"
	"mini-k8s/pkg/controller/nodelifecycle"
	"mini-k8s/pkg/controller/replicaset"
	"mini-k8s/pkg/controller/tainteviction"
)

// runControllerManager runs every controller against shared informers.
func runControllerManager(ctx context.Context, c *client.Client, _ string) {
	host, _ := os.Hostname()
	rec := record.New(c, "kube-controller-manager", host)
	pods := informers.New(client.Pods(c, ""), client.ListOptions{}, 30*time.Second)
	deployments := informers.New(client.Deployments(c, ""), client.ListOptions{}, 30*time.Second)
	rsets := informers.New(client.ReplicaSets(c, ""), client.ListOptions{}, 30*time.Second)
	namespaces := informers.New(client.Namespaces(c), client.ListOptions{}, 30*time.Second)
	nodes := informers.New(client.Nodes(c), client.ListOptions{}, 30*time.Second)
	services := informers.New(client.Services(c, ""), client.ListOptions{}, 30*time.Second)
	endpoints := informers.New(client.Endpoints(c, ""), client.ListOptions{}, 30*time.Second)
	leases := informers.New(client.Leases(c, corev1.NamespaceNodeLease), client.ListOptions{}, 0)
	allocator, err := nodeipam.NewAllocator(env("MINI_CLUSTER_CIDR", nodeipam.DefaultClusterCIDR), 24)
	if err != nil {
		log.Fatal(err)
	}
	dc := deployment.NewDeploymentController(c, deployments, rsets, pods, rec)
	rc := replicaset.NewController(c, rsets, pods, rec)
	ec := endpoint.NewController(c, services, pods, endpoints)
	nc := namespace.NewNamespaceController(c, namespaces)
	ipam := nodeipam.NewController(c, nodes, allocator, rec)
	lifecycle := nodelifecycle.NewController(c, nodes, leases, pods, rec)
	eviction := tainteviction.NewController(c, nodes, pods, rec)
	gc := garbagecollector.NewGarbageCollector(c, garbagecollector.TrackedResources())
	go rec.Run(ctx)
	for _, run := range []func(context.Context){pods.Run, deployments.Run, rsets.Run, namespaces.Run, nodes.Run, services.Run, endpoints.Run, leases.Run} {
		go run(ctx)
	}
	informers.WaitForSync(ctx, pods.HasSynced, deployments.HasSynced, rsets.HasSynced, namespaces.HasSynced, nodes.HasSynced,
		services.HasSynced, endpoints.HasSynced, leases.HasSynced)
	go rc.Run(ctx, 4)
	go ec.Run(ctx, 4)
	go nc.Run(ctx, 2)
	go ipam.Run(ctx)
	go lifecycle.Run(ctx)
	go eviction.Run(ctx, 2)
	go gc.Run(ctx)
	log.Print("kube-controller-manager started")
	dc.Run(ctx, 4)
}
