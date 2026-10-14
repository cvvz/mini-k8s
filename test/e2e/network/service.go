// Package network holds Pod network, Service and Ingress specs.
package network

import (
	"fmt"
	"net"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

// hostnameOf extracts the Pod name from a demo response ("200\nhello from X").
func hostnameOf(out string) string {
	_, host, _ := strings.Cut(out, "hello from ")
	return strings.TrimSpace(host)
}

var _ = framework.SIGDescribe("network", "Services", func() {
	f := framework.NewDefaultFramework("svc")

	service := func(ctx SpecContext, name string) *corev1.Service {
		s, err := client.Services(f.Client, f.Namespace).Get(ctx, name)
		framework.ExpectNoError(err)
		return s
	}
	// clientPod starts an idle Pod to send requests from.
	clientPod := func(ctx SpecContext) corev1.Pod {
		labels := map[string]string{"role": "client"}
		f.Apply(ctx, framework.NewPod("client", labels, framework.MiniContainer("client", "sleep", "1h")))
		return f.WaitForRunningPods(ctx, labels, 1)[0]
	}
	// reachAll fetches url from pod until every backend has answered.
	reachAll := func(ctx SpecContext, pod, url string, backends []corev1.Pod) {
		seen := map[string]bool{}
		Eventually(ctx, func(g Gomega) {
			out, err := f.FetchFromPod(ctx, pod, url)
			g.Expect(err).NotTo(HaveOccurred(), out)
			g.Expect(out).To(HavePrefix("200\n"))
			seen[hostnameOf(out)] = true
			for _, b := range backends {
				g.Expect(seen).To(HaveKey(b.Name), "no answer from %s yet", b.Name)
			}
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(Succeed())
	}

	It("should give a Service a cluster IP that balances over its endpoints from every node", func(ctx SpecContext) {
		labels := map[string]string{"app": "lb"}
		f.Apply(ctx, framework.NewDeployment("lb", 2, labels, framework.DemoContainer()), framework.NewService("lb", labels, 8080))
		backends := f.WaitForRunningPods(ctx, labels, 2)
		from := clientPod(ctx)

		svc := service(ctx, "lb")
		_, serviceCIDR, _ := net.ParseCIDR("10.96.0.0/12")
		Expect(serviceCIDR.Contains(net.ParseIP(svc.Spec.ClusterIP))).To(BeTrue(), svc.Spec.ClusterIP)
		Expect(f.KK(ctx, "get", "svc", "lb")).To(MatchRegexp(`lb\s+ClusterIP\s+` + svc.Spec.ClusterIP + `\s+<none>\s+80/TCP`))
		Eventually(ctx, func() string { return f.KK(ctx, "get", "endpoints", "lb") }).Should(And(
			ContainSubstring(backends[0].Status.PodIP+":8080"), ContainSubstring(backends[1].Status.PodIP+":8080")))

		reachAll(ctx, from.Name, "http://"+svc.Spec.ClusterIP+"/", backends)
		By("keeping the client's address (no SNAT between Pods)")
		Eventually(ctx, func() string {
			logs, _ := framework.RunKK(ctx, "logs", "-n", f.Namespace, backends[0].Name)
			logs2, _ := framework.RunKK(ctx, "logs", "-n", f.Namespace, backends[1].Name)
			return logs + logs2
		}).Should(ContainSubstring("from " + from.Status.PodIP + ":"))
	})

	It("should resolve named target ports and serve several ports", func(ctx SpecContext) {
		labels := map[string]string{"app": "ports"}
		c := framework.DemoContainer()
		c.Fields = map[string]any{"ports": []any{map[string]any{"name": "http", "containerPort": 8080}}}
		svc := framework.NewService("ports", labels, 8080)
		svc.Spec()["ports"] = []any{
			map[string]any{"name": "web", "port": 80, "targetPort": "http"},
			map[string]any{"name": "alt", "port": 8888, "targetPort": 8080},
		}
		f.Apply(ctx, framework.NewDeployment("ports", 1, labels, c), svc)
		backends := f.WaitForRunningPods(ctx, labels, 1)
		from := clientPod(ctx)
		ip := service(ctx, "ports").Spec.ClusterIP
		Expect(f.KK(ctx, "get", "svc", "ports")).To(MatchRegexp(`80/TCP,8888/TCP`))
		reachAll(ctx, from.Name, "http://"+ip+"/", backends)
		reachAll(ctx, from.Name, "http://"+ip+":8888/", backends)
	})

	It("should let a Pod reach itself through its own Service", func(ctx SpecContext) {
		labels := map[string]string{"app": "hairpin"}
		f.Apply(ctx, framework.NewDeployment("hairpin", 1, labels, framework.DemoContainer()), framework.NewService("hairpin", labels, 8080))
		self := f.WaitForRunningPods(ctx, labels, 1)
		reachAll(ctx, self[0].Name, "http://"+service(ctx, "hairpin").Spec.ClusterIP+"/", self)
	})

	It("should refuse connections at once while a Service has no ready endpoints", func(ctx SpecContext) {
		f.Apply(ctx, framework.NewService("nobody", map[string]string{"app": "nobody"}, 8080))
		from := clientPod(ctx)
		url := "http://" + service(ctx, "nobody").Spec.ClusterIP + "/"
		Eventually(ctx, func() string {
			start := time.Now()
			out, _ := f.FetchFromPod(ctx, from.Name, url)
			if time.Since(start) > 5*time.Second {
				return "too slow: " + out
			}
			return out
		}).WithTimeout(20 * time.Second).Should(ContainSubstring("connection refused"))
		Expect(f.KK(ctx, "get", "endpoints", "nobody")).To(MatchRegexp(`nobody\s+<none>`))
	})

	It("should keep a client on one backend with ClientIP session affinity", func(ctx SpecContext) {
		labels := map[string]string{"app": "sticky"}
		svc := framework.NewService("sticky", labels, 8080)
		svc.Spec()["sessionAffinity"] = "ClientIP"
		f.Apply(ctx, framework.NewDeployment("sticky", 3, labels, framework.DemoContainer()), svc)
		f.WaitForRunningPods(ctx, labels, 3)
		Eventually(ctx, func(g Gomega) {
			e, err := client.Endpoints(f.Client, f.Namespace).Get(ctx, "sticky")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(e.Subsets).To(HaveLen(1))
			g.Expect(e.Subsets[0].Addresses).To(HaveLen(3))
		}).WithTimeout(30 * time.Second).Should(Succeed())
		from := clientPod(ctx)
		url := "http://" + service(ctx, "sticky").Spec.ClusterIP + "/"
		Eventually(ctx, func() (string, error) { return f.FetchFromPod(ctx, from.Name, url) }).Should(HavePrefix("200\n"))
		seen := map[string]int{}
		for range 10 {
			out, err := f.FetchFromPod(ctx, from.Name, url)
			framework.ExpectNoError(err)
			seen[hostnameOf(out)]++
		}
		Expect(seen).To(HaveLen(1), "every request of the client goes to the same Pod")
	})

	It("should move a Pod to notReadyAddresses while its readiness probe fails", func(ctx SpecContext) {
		labels := map[string]string{"app": "gated"}
		c := framework.DemoContainer()
		c.Fields = map[string]any{"readinessProbe": map[string]any{
			"httpGet": map[string]any{"path": "/ready", "port": 8080}, "periodSeconds": 1, "failureThreshold": 1}}
		f.Apply(ctx, framework.NewDeployment("gated", 2, labels, c), framework.NewService("gated", labels, 8080))
		pods := f.WaitForRunningPods(ctx, labels, 2)
		victim, other := pods[0], pods[1]
		endpoints := func(g Gomega) *corev1.Endpoints {
			e, err := client.Endpoints(f.Client, f.Namespace).Get(ctx, "gated")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(e.Subsets).To(HaveLen(1))
			return e
		}
		ips := func(list []corev1.EndpointAddress) []string {
			var out []string
			for _, a := range list {
				out = append(out, a.IP)
			}
			return out
		}
		Eventually(ctx, func(g Gomega) {
			g.Expect(ips(endpoints(g).Subsets[0].Addresses)).To(ConsistOf(victim.Status.PodIP, other.Status.PodIP))
		}).WithTimeout(30 * time.Second).Should(Succeed())

		By("failing the readiness probe of one Pod")
		_, err := f.FetchFromPod(ctx, victim.Name, "http://127.0.0.1:8080/fail-ready")
		framework.ExpectNoError(err)
		Eventually(ctx, func(g Gomega) {
			e := endpoints(g)
			g.Expect(ips(e.Subsets[0].Addresses)).To(ConsistOf(other.Status.PodIP))
			g.Expect(ips(e.Subsets[0].NotReadyAddresses)).To(ConsistOf(victim.Status.PodIP))
		}).WithTimeout(30 * time.Second).Should(Succeed())
		url := "http://" + service(ctx, "gated").Spec.ClusterIP + "/"
		// kube-proxy follows the Endpoints within about a second; then every
		// request goes to the ready Pod.
		Eventually(ctx, func(g Gomega) {
			for range 6 {
				out, err := f.FetchFromPod(ctx, other.Name, url)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(hostnameOf(out)).To(Equal(other.Name), "the unready Pod gets no Service traffic")
			}
		}).WithTimeout(20 * time.Second).Should(Succeed())

		By("recovering")
		_, err = f.FetchFromPod(ctx, victim.Name, "http://127.0.0.1:8080/recover")
		framework.ExpectNoError(err)
		Eventually(ctx, func(g Gomega) {
			g.Expect(ips(endpoints(g).Subsets[0].Addresses)).To(ConsistOf(victim.Status.PodIP, other.Status.PodIP))
		}).WithTimeout(30 * time.Second).Should(Succeed())
	})

	It("should route to the Endpoints a user maintains for a Service without a selector", func(ctx SpecContext) {
		labels := map[string]string{"app": "target"}
		f.Apply(ctx, framework.NewPod("target", labels, framework.DemoContainer()))
		target := f.WaitForRunningPods(ctx, labels, 1)
		svc := framework.NewService("manual", nil, 8080)
		delete(svc.Spec(), "selector")
		f.Apply(ctx, svc, framework.Object{"apiVersion": "v1", "kind": "Endpoints", "metadata": map[string]any{"name": "manual"},
			"subsets": []any{map[string]any{"addresses": []any{map[string]any{"ip": target[0].Status.PodIP}},
				"ports": []any{map[string]any{"port": 8080}}}}})
		from := clientPod(ctx)
		reachAll(ctx, from.Name, "http://"+service(ctx, "manual").Spec.ClusterIP+"/", target)
	})

	It("should expose a NodePort Service on every node", func(ctx SpecContext) {
		labels := map[string]string{"app": "np"}
		svc := framework.NewService("np", labels, 8080)
		svc.Spec()["type"] = "NodePort"
		f.Apply(ctx, framework.NewDeployment("np", 1, labels, framework.DemoContainer()), svc)
		backend := f.WaitForRunningPods(ctx, labels, 1)[0]
		port := service(ctx, "np").Spec.Ports[0].NodePort
		Expect(port).To(BeNumerically(">=", 30000))
		Expect(f.KK(ctx, "get", "svc", "np")).To(MatchRegexp(fmt.Sprintf(`np\s+NodePort\s+\S+\s+<none>\s+80:%d/TCP`, port)))
		nodes, err := client.Nodes(f.Client).List(ctx, client.ListOptions{})
		framework.ExpectNoError(err)
		Expect(nodes.Items).To(HaveLen(len(framework.Nodes)))
		for _, n := range nodes.Items {
			Eventually(ctx, func() (string, error) {
				_, body, err := framework.HTTPGet(ctx, fmt.Sprintf("http://%s:%d/", n.InternalIP(), port))
				return body, err
			}).WithTimeout(30*time.Second).Should(Equal("hello from "+backend.Name+"\n"), "through node %s", n.Name)
		}
	})

	It("should report allocation errors like Kubernetes and release what Services give up", func(ctx SpecContext) {
		// Explicit values come from the static bands, which dynamic
		// allocation leaves alone, so parallel specs cannot take them.
		ip, nodePort := "10.96.0.231", 30031
		pinned := framework.NewService("pinned", map[string]string{"app": "x"}, 8080)
		pinned.Spec()["clusterIP"] = ip
		pinned.Spec()["type"] = "NodePort"
		pinned.Spec()["ports"] = []any{map[string]any{"port": 80, "targetPort": 8080, "nodePort": nodePort}}
		f.Apply(ctx, pinned)
		Expect(f.KK(ctx, "get", "svc", "pinned")).To(MatchRegexp(ip + fmt.Sprintf(`\s+<none>\s+80:%d/TCP`, nodePort)))

		dup := framework.NewService("dup", map[string]string{"app": "x"}, 8080)
		dup.Spec()["clusterIP"] = ip
		Expect(f.ApplyExpectError(ctx, dup)).To(ContainSubstring(
			fmt.Sprintf(`spec.clusterIP: Invalid value: %q: failed to allocate IP %s: provided IP is already allocated`, ip, ip)))
		dup.Spec()["clusterIP"] = "10.200.0.1"
		Expect(f.ApplyExpectError(ctx, dup)).To(ContainSubstring("the provided network does not match the current range"))
		delete(dup.Spec(), "clusterIP")
		dup.Spec()["type"] = "NodePort"
		dup.Spec()["ports"] = []any{map[string]any{"port": 80, "nodePort": nodePort}}
		Expect(f.ApplyExpectError(ctx, dup)).To(ContainSubstring(
			fmt.Sprintf("spec.ports[0].nodePort: Invalid value: %d: provided port is already allocated", nodePort)))
		dup.Spec()["ports"] = []any{map[string]any{"port": 80, "nodePort": 40000}}
		Expect(f.ApplyExpectError(ctx, dup)).To(ContainSubstring(
			"provided port is not in the valid range. The range of valid ports is 30000-32767"))
		dup.Spec()["type"] = "ClusterIP"
		Expect(f.ApplyExpectError(ctx, dup)).To(ContainSubstring("spec.ports[0].nodePort: Forbidden: may not be used when `type` is 'ClusterIP'"))

		changed := framework.NewService("pinned", map[string]string{"app": "x"}, 8080)
		changed.Spec()["clusterIP"] = "10.96.0.232"
		Expect(f.ApplyExpectError(ctx, changed)).To(ContainSubstring(`spec.clusterIP: Invalid value: "10.96.0.232": may not change once set`))

		By("switching to ClusterIP, which releases the node port")
		pinned.Spec()["type"] = "ClusterIP"
		pinned.Spec()["ports"] = []any{map[string]any{"port": 80, "targetPort": 8080}}
		f.Apply(ctx, pinned)
		Expect(service(ctx, "pinned").Spec.Ports[0].NodePort).To(BeZero())
		dup.Spec()["type"] = "NodePort"
		dup.Spec()["ports"] = []any{map[string]any{"port": 80, "nodePort": nodePort}}
		f.Apply(ctx, dup)

		By("deleting the Service, which releases its cluster IP")
		framework.KK(ctx, "delete", "-n", f.Namespace, "svc", "pinned")
		again := framework.NewService("again", map[string]string{"app": "x"}, 8080)
		again.Spec()["clusterIP"] = ip
		f.Apply(ctx, again)
	})
})

var _ = framework.SIGDescribe("network", "kube-proxy", func() {
	f := framework.NewDefaultFramework("kube-proxy")

	It("should run on every node as a static Pod", func(ctx SpecContext) {
		for _, node := range framework.Nodes {
			p, err := client.Pods(f.Client, "kube-system").Get(ctx, "kube-proxy-"+node)
			framework.ExpectNoError(err, node)
			Expect(corev1.IsMirrorPod(p)).To(BeTrue())
			Expect(p.Spec.HostNetwork).To(BeTrue())
			Expect(*p.Spec.Containers[0].SecurityContext.Privileged).To(BeTrue())
			Expect(corev1.IsPodReady(p)).To(BeTrue(), node)
		}
		out, err := framework.Docker(ctx, "exec", framework.ComponentContainer("worker-1"), "iptables", "-t", "nat", "-S", "PREROUTING")
		framework.ExpectNoError(err)
		Expect(out).To(ContainSubstring(`-m comment --comment "kubernetes service portals" -j KUBE-SERVICES`))
	})

	It("should restore Service rules that are removed behind its back", framework.WithDisruptive(), Serial, func(ctx SpecContext) {
		labels := map[string]string{"app": "heal"}
		f.Apply(ctx, framework.NewDeployment("heal", 1, labels, framework.DemoContainer()), framework.NewService("heal", labels, 8080))
		backend := f.WaitForRunningPods(ctx, labels, 1)[0]
		svc, err := client.Services(f.Client, f.Namespace).Get(ctx, "heal")
		framework.ExpectNoError(err)
		url := "http://" + svc.Spec.ClusterIP + "/"
		fetch := func() (string, error) { return f.FetchFromPod(ctx, backend.Name, url) }
		Eventually(ctx, fetch).WithTimeout(30 * time.Second).Should(HavePrefix("200\n"))

		By("flushing KUBE-SERVICES on the backend's node")
		_, err = framework.Docker(ctx, "exec", framework.ComponentContainer(backend.Spec.NodeName), "iptables", "-t", "nat", "-F", "KUBE-SERVICES")
		framework.ExpectNoError(err)
		Eventually(ctx, fetch).WithTimeout(45*time.Second).WithPolling(time.Second).Should(HavePrefix("200\n"),
			"kube-proxy's periodic resync writes the rules again")
	})
})
