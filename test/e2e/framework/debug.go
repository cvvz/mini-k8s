package framework

import (
	"context"

	"github.com/onsi/ginkgo/v2"
)

// ControlPlaneComponents are the static Pods of the control-plane node;
// kube-proxy also runs on every worker.
var ControlPlaneComponents = []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "ingress-controller"}

// DumpDebugInfo writes cluster state, the namespace's objects and events, and
// recent component logs to the spec report.
func DumpDebugInfo(ctx context.Context, namespace string) {
	w := ginkgo.GinkgoWriter
	w.Println("===== mini-k8s debug dump =====")
	commands := [][]string{{"get", "nodes", "-o", "wide"}}
	if namespace != "" {
		commands = append(commands,
			[]string{"get", "deployments,pods,services,endpoints,ingresses", "-o", "wide", "-n", namespace},
			[]string{"get", "events", "-n", namespace})
	}
	for _, args := range commands {
		out, err := RunKK(ctx, args...)
		w.Printf("$ kk %v\n%s", args, out)
		if err != nil {
			w.Printf("error: %v\n", err)
		}
	}
	nodes, err := RunningNodes(ctx)
	if err != nil {
		w.Printf("error: %v\n", err)
	}
	for _, node := range nodes {
		out, _ := Crictl(ctx, node, "ps", "-a")
		w.Printf("$ crictl ps -a (on %s)\n%s", node, out)
		out, _ = Docker(ctx, "logs", "--tail", "15", ComponentContainer(node))
		w.Printf("----- node %s: containerd and kubelet (last 15 lines) -----\n%s", node, out)
	}
	for _, c := range ControlPlaneComponents {
		out, _ := RunKK(ctx, "logs", "-n", "kube-system", c+"-control-plane", "--tail", "15")
		w.Printf("----- %s (last 15 lines) -----\n%s", c, out)
	}
}
