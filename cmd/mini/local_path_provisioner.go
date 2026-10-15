package main

import (
	"context"
	"log"
	"os"
	"time"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/volume/localpath"
)

// runLocalPathProvisioner provisions node-local volumes for claims of
// StorageClasses with the mini-k8s.io/local-path provisioner; it runs as
// the local-path-storage add-on, whose namespace it runs helper Pods in.
func runLocalPathProvisioner(ctx context.Context, c *client.Client, _ string) {
	host, _ := os.Hostname()
	rec := record.New(c, localpath.Name, host)
	claims := informers.New(client.PersistentVolumeClaims(c, ""), client.ListOptions{}, 30*time.Second)
	volumes := informers.New(client.PersistentVolumes(c), client.ListOptions{}, 30*time.Second)
	classes := informers.New(client.StorageClasses(c), client.ListOptions{}, 30*time.Second)
	p := localpath.New(c, claims, volumes, classes, rec, env("POD_NAMESPACE", "local-path-storage"))
	go rec.Run(ctx)
	for _, run := range []func(context.Context){claims.Run, volumes.Run, classes.Run} {
		go run(ctx)
	}
	if !informers.WaitForSync(ctx, claims.HasSynced, volumes.HasSynced, classes.HasSynced) {
		return
	}
	log.Print("local-path-provisioner started")
	p.Run(ctx, 2)
}
