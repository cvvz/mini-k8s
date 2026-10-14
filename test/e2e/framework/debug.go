package framework

import (
	"context"

	"github.com/onsi/ginkgo/v2"
)

// ControlPlaneComponents are the Compose services whose logs help diagnose failures.
var ControlPlaneComponents = []string{"apiserver", "scheduler", "controller", "worker-1", "worker-2", "proxy"}

// DumpDebugInfo writes cluster state, the namespace's objects and events, and
// recent component logs to the spec report.
func DumpDebugInfo(ctx context.Context, namespace string) {
	w := ginkgo.GinkgoWriter
	w.Println("===== mini-k8s debug dump =====")
	commands := [][]string{{"get", "nodes", "-o", "wide"}}
	if namespace != "" {
		commands = append(commands,
			[]string{"get", "deployments,pods,services", "-o", "wide", "-n", namespace},
			[]string{"get", "events", "-n", namespace})
	}
	for _, args := range commands {
		out, err := RunKK(ctx, args...)
		w.Printf("$ kk %v\n%s", args, out)
		if err != nil {
			w.Printf("error: %v\n", err)
		}
	}
	out, _ := Docker(ctx, "ps", "-a", "--filter", "label=mini.cluster=mini-k8s", "--format", "{{.Names}}\t{{.Status}}")
	w.Printf("$ docker ps (workload containers)\n%s", out)
	for _, c := range ControlPlaneComponents {
		out, _ := Docker(ctx, "logs", "--tail", "15", ComponentContainer(c))
		w.Printf("----- %s (last 15 lines) -----\n%s", c, out)
	}
}
