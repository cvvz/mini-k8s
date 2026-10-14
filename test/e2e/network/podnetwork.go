package network

import (
	"net"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

// clusterCIDR is the default Pod address space split among the nodes.
const clusterCIDR = "10.244.0.0/16"

var _ = framework.SIGDescribe("network", "Pod network", func() {
	f := framework.NewDefaultFramework("podnet")

	// pinnedPod runs the demo app on a chosen node, bypassing the scheduler.
	pinnedPod := func(name, node string) framework.Object {
		obj := framework.NewPod(name, map[string]string{"app": name}, framework.DemoContainer())
		obj.Spec()["nodeName"] = node
		return obj
	}

	It("should give every node a distinct Pod CIDR and Pods an IP from it", func(ctx SpecContext) {
		_, cluster, _ := net.ParseCIDR(clusterCIDR)
		nodes, err := client.Nodes(f.Client).List(ctx, client.ListOptions{})
		framework.ExpectNoError(err)
		cidrs := map[string]*net.IPNet{}
		seen := map[string]string{}
		for _, n := range nodes.Items {
			Expect(n.Spec.PodCIDR).NotTo(BeEmpty(), "node %s has no Pod CIDR", n.Name)
			Expect(seen).NotTo(HaveKey(n.Spec.PodCIDR), "Pod CIDR %s assigned twice", n.Spec.PodCIDR)
			seen[n.Spec.PodCIDR] = n.Name
			ip, subnet, err := net.ParseCIDR(n.Spec.PodCIDR)
			framework.ExpectNoError(err)
			Expect(cluster.Contains(ip)).To(BeTrue(), "%s is outside %s", n.Spec.PodCIDR, clusterCIDR)
			cidrs[n.Name] = subnet
		}
		Expect(framework.KK(ctx, "get", "node", "worker-1", "-o", "yaml")).To(ContainSubstring("podCIDR: "))

		f.Apply(ctx, pinnedPod("ip-1", "worker-1"), pinnedPod("ip-2", "worker-2"))
		for _, name := range []string{"ip-1", "ip-2"} {
			pod := f.WaitForRunningPods(ctx, map[string]string{"app": name}, 1)[0]
			Expect(cidrs[pod.Spec.NodeName].Contains(net.ParseIP(pod.Status.PodIP))).To(BeTrue(),
				"Pod IP %s is outside node %s's range %s", pod.Status.PodIP, pod.Spec.NodeName, cidrs[pod.Spec.NodeName])
			Expect(pod.Status.HostIP).NotTo(Equal(pod.Status.PodIP))
		}
	})

	It("should connect Pods across nodes without NAT", func(ctx SpecContext) {
		f.Apply(ctx, pinnedPod("client", "worker-1"), pinnedPod("server", "worker-2"))
		src := f.WaitForRunningPods(ctx, map[string]string{"app": "client"}, 1)[0]
		dst := f.WaitForRunningPods(ctx, map[string]string{"app": "server"}, 1)[0]

		By("fetching the server Pod's IP from the client Pod on the other node")
		Eventually(ctx, func() (string, error) {
			return framework.ExecInPod(ctx, src, "/mini", "fetch", "http://"+dst.Status.PodIP+":8080/cross-node")
		}).WithTimeout(30 * time.Second).Should(Equal("200\nhello from server\n"))

		By("checking the server saw the client's Pod IP as the source")
		Eventually(ctx, func() (string, error) {
			return framework.RunKK(ctx, "logs", "server", "-n", f.Namespace)
		}).Should(ContainSubstring("GET /cross-node from " + src.Status.PodIP + ":"))
	})

	It("should resolve names through the node and reach destinations outside the cluster", func(ctx SpecContext) {
		f.Apply(ctx, framework.NewPod("egress", map[string]string{"app": "egress"}, framework.DemoContainer()))
		pod := f.WaitForRunningPods(ctx, map[string]string{"app": "egress"}, 1)[0]

		By("resolving the apiserver's Compose name and calling it without credentials")
		Eventually(ctx, func() (string, error) {
			out, err := framework.ExecInPod(ctx, pod, "/mini", "fetch", "http://apiserver:8080/healthz")
			return strings.SplitN(out, "\n", 2)[0], err
		}).WithTimeout(30*time.Second).Should(Equal("401"), "the API answers 401 to anonymous requests")
	})

	It("should keep host-gw routes to every other node's Pod CIDR", func(ctx SpecContext) {
		nodes, err := client.Nodes(f.Client).List(ctx, client.ListOptions{})
		framework.ExpectNoError(err)
		byName := map[string]*corev1.Node{}
		for i := range nodes.Items {
			byName[nodes.Items[i].Name] = &nodes.Items[i]
		}
		for _, self := range framework.Workers {
			routes, err := framework.Docker(ctx, "exec", framework.ComponentContainer(self), "ip", "route", "show")
			framework.ExpectNoError(err)
			for _, other := range framework.Workers {
				if other == self {
					continue
				}
				Expect(routes).To(ContainSubstring(byName[other].Spec.PodCIDR+" via "+byName[other].InternalIP()),
					"route on %s to %s's Pods", self, other)
			}
		}
	})
})
