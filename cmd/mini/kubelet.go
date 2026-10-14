package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/kubelet"
	"mini-k8s/pkg/kubelet/dockershim"
)

// runKubelet runs the node agent for MINI_NODE and serves its API on :10250.
func runKubelet(ctx context.Context, c *client.Client, token string) {
	node := os.Getenv("MINI_NODE")
	if node == "" {
		log.Fatal("MINI_NODE is required")
	}
	rt, err := dockershim.NewRuntime("/var/run/docker.sock", env("MINI_CLUSTER", "mini-k8s"), node, env("MINI_NETWORK", "mini-k8s"))
	if err != nil {
		log.Fatal(err)
	}
	address, err := nodeAddress()
	if err != nil {
		log.Fatal(err)
	}
	rec := record.New(c, "kubelet", node)
	k := kubelet.New(c, rt, rec, node, address, token)
	go rec.Run(ctx)
	go k.Run(ctx)
	serve(ctx, ":10250", k.Handler())
}

// nodeAddress returns this container's IPv4 address on the cluster network.
func nodeAddress() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no IPv4 address found")
}
