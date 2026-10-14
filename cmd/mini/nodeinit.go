package main

import (
	"context"
	"log"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/nodeinit"
)

// runNodeInit is the entrypoint of a node container: it prepares the node,
// starts containerd and runs the kubelet in-process.
func runNodeInit(ctx context.Context, c *client.Client, token string) {
	err := nodeinit.Run(ctx, nodeinit.Options{
		SandboxImage:     env("MINI_SANDBOX_IMAGE", nodeinit.DefaultSandboxImage),
		CgroupRoot:       "/sys/fs/cgroup",
		ContainerdConfig: nodeinit.DefaultContainerdConfig,
		ContainerdSocket: nodeinit.DefaultContainerdSocket,
		Kubelet: func(ctx context.Context) error {
			runKubelet(ctx, c, token)
			return nil
		},
	})
	if err != nil {
		log.Fatalf("node-init: %v", err)
	}
}
