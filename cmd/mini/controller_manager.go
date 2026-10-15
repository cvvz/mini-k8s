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
	"mini-k8s/pkg/controller/podgc"
	"mini-k8s/pkg/controller/replicaset"
	"mini-k8s/pkg/controller/tainteviction"
	"mini-k8s/pkg/controller/volume/persistentvolume"
	"mini-k8s/pkg/controller/volume/pvcprotection"
	"mini-k8s/pkg/controller/volume/pvprotection"
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
	volumes := informers.New(client.PersistentVolumes(c), client.ListOptions{}, 30*time.Second)
	claims := informers.New(client.PersistentVolumeClaims(c, ""), client.ListOptions{}, 30*time.Second)
	classes := informers.New(client.StorageClasses(c), client.ListOptions{}, 30*time.Second)
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
	podGC := podgc.NewController(c, pods, nodes)
	pvBinder := persistentvolume.NewController(c, volumes, claims, classes, rec)
	pvcProtection := pvcprotection.NewController(c, claims, pods)
	pvProtection := pvprotection.NewController(c, volumes)
	go rec.Run(ctx)
	for _, run := range []func(context.Context){pods.Run, deployments.Run, rsets.Run, namespaces.Run, nodes.Run, services.Run,
		endpoints.Run, leases.Run, volumes.Run, claims.Run, classes.Run} {
		go run(ctx)
	}
	informers.WaitForSync(ctx, pods.HasSynced, deployments.HasSynced, rsets.HasSynced, namespaces.HasSynced, nodes.HasSynced,
		services.HasSynced, endpoints.HasSynced, leases.HasSynced, volumes.HasSynced, claims.HasSynced, classes.HasSynced)
	go rc.Run(ctx, 4)
	go ec.Run(ctx, 4)
	go nc.Run(ctx, 2)
	go ipam.Run(ctx)
	go lifecycle.Run(ctx)
	go eviction.Run(ctx, 2)
	go gc.Run(ctx)
	go podGC.Run(ctx)
	go pvBinder.Run(ctx, 2)
	go pvcProtection.Run(ctx, 2)
	go pvProtection.Run(ctx, 1)
	log.Print("kube-controller-manager started")
	dc.Run(ctx, 4)
}
