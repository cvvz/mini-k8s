package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"os"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/leaderelection"
	"mini-k8s/pkg/client/record"
)

// runLeaderElected runs a component only while it holds the Lease name in
// kube-system, as kube-controller-manager and kube-scheduler do with
// --leader-elect (MINI_LEADER_ELECT=false runs it at once). Losing the
// Lease ends the process, so the kubelet restarts it as a candidate; a
// shutdown releases the Lease so another replica takes over at once.
func runLeaderElected(ctx context.Context, c *client.Client, name string, run func(context.Context)) {
	if env("MINI_LEADER_ELECT", "true") == "false" {
		run(ctx)
		return
	}
	host, _ := os.Hostname()
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80 // a version 4 UUID
	id := fmt.Sprintf("%s_%x-%x-%x-%x-%x", host, b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
	rec := record.New(c, name, host)
	go rec.Run(context.WithoutCancel(ctx))
	log.Printf("%s: attempting to acquire leader lease kube-system/%s as %s", name, name, id)
	leaderelection.Run(ctx, leaderelection.Config{Client: c, Namespace: "kube-system", Name: name, Identity: id,
		LeaseDuration: leaderelection.DefaultLeaseDuration, RenewDeadline: leaderelection.DefaultRenewDeadline,
		RetryPeriod: leaderelection.DefaultRetryPeriod, ReleaseOnCancel: true, Recorder: rec,
		Callbacks: leaderelection.Callbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() {
				if ctx.Err() == nil {
					log.Fatalf("%s: leaderelection lost", name)
				}
			},
			OnNewLeader: func(leader string) {
				if leader != id {
					log.Printf("%s: new leader elected: %s", name, leader)
				}
			},
		}})
}
