package framework

import (
	"context"

	"github.com/onsi/ginkgo/v2"
)

// ControlPlaneComponents are the Compose services whose logs help diagnose failures.
var ControlPlaneComponents = []string{"apiserver", "scheduler", "controller", "worker-1", "worker-2", "proxy"}

// DumpDebugInfo writes cluster state and recent component logs to the spec report.
func DumpDebugInfo(ctx context.Context) {
	w := ginkgo.GinkgoWriter
	w.Println("===== mini-k8s debug dump =====")
	for _, kind := range []string{"nodes", "deployments", "pods", "services"} {
		out, err := RunKK(ctx, "get", kind)
		w.Printf("$ kk get %s\n%s", kind, out)
		if err != nil {
			w.Printf("error: %v\n", err)
		}
	}
	out, _ := Docker(ctx, "ps", "-a", "--filter", "label=mini.cluster=mini-k8s", "--format", "{{.Names}}\t{{.Status}}\t{{.Label \"mini.pod\"}}")
	w.Printf("$ docker ps (workload containers)\n%s", out)
	for _, c := range ControlPlaneComponents {
		out, _ := Docker(ctx, "logs", "--tail", "15", ComponentContainer(c))
		w.Printf("----- %s (last 15 lines) -----\n%s", c, out)
	}
}
