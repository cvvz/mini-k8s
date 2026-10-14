package main

import (
	"context"
	"log"
	"time"

	"mini-k8s/pkg/apiserver"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/registry"
	"mini-k8s/pkg/storage/etcd3"
)

// runAPIServer serves the API on :8080 once etcd is reachable.
func runAPIServer(ctx context.Context, _ *client.Client, token string) {
	store := etcd3.New(env("MINI_ETCD", "http://etcd:2379"))
	reg := registry.New(store, "")
	s := apiserver.New(apiserver.Config{Store: store, Token: token, Strategies: reg.Strategies})
	for {
		err := s.Bootstrap(ctx)
		if err == nil {
			break
		}
		log.Printf("kube-apiserver: waiting for etcd: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	go reg.ServiceRepair.Run(ctx, time.Minute)
	serve(ctx, ":8080", s)
}
