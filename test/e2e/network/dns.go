package network

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("network", "DNS", func() {
	f := framework.NewDefaultFramework("dns")

	inPod := func(ctx SpecContext, pod string, args ...string) (string, error) {
		return framework.RunKK(ctx, append([]string{"exec", "-n", f.Namespace, pod, "--", "/mini"}, args...)...)
	}
	// resolves waits until `mini lookup ARGS` in the Pod prints want.
	resolves := func(ctx SpecContext, pod string, want string, args ...string) {
		GinkgoHelper()
		Eventually(ctx, func() (string, error) {
			return inPod(ctx, pod, append([]string{"lookup"}, args...)...)
		}).WithTimeout(30*time.Second).Should(Equal(want), "lookup %v", args)
	}
	clientPod := func(ctx SpecContext) corev1.Pod {
		GinkgoHelper()
		labels := map[string]string{"role": "dns-client"}
		f.Apply(ctx, framework.NewPod("client", labels, framework.MiniContainer("client", "sleep", "1h")))
		return f.WaitForRunningPods(ctx, labels, 1)[0]
	}

	It("should run the cluster DNS add-on behind kube-dns", func(ctx SpecContext) {
		Expect(framework.KK(ctx, "get", "svc", "kube-dns", "-n", "kube-system")).To(
			MatchRegexp(`kube-dns\s+ClusterIP\s+10\.96\.0\.10\s+<none>\s+53/UDP,53/TCP`))
		Eventually(ctx, func() string { return framework.KK(ctx, "get", "deployment", "kube-dns", "-n", "kube-system") }).
			Should(MatchRegexp(`kube-dns\s+2/2\s+2\s+2`))
		Expect(framework.KK(ctx, "get", "svc", "kubernetes", "-n", "default")).To(
			MatchRegexp(`kubernetes\s+ClusterIP\s+10\.96\.0\.1\s+<none>\s+443/TCP`))
	})

	It("should resolve Services, headless endpoints, SRV records, ExternalNames and Pods", func(ctx SpecContext) {
		web := map[string]string{"app": "web"}
		hl := map[string]string{"app": "hl"}
		member := framework.NewPod("member", hl, framework.DemoContainer())
		member.Spec()["hostname"], member.Spec()["subdomain"] = "member-0", "hl"
		headless := framework.NewService("hl", hl, 8080)
		headless.Spec()["clusterIP"] = "None"
		headless.Spec()["ports"] = []any{map[string]any{"name": "http", "port": 80, "targetPort": 8080}}
		svc := framework.NewService("web", web, 8080)
		svc.Spec()["ports"] = []any{map[string]any{"name": "http", "port": 80, "targetPort": 8080}}
		alias := framework.Object{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "alias"},
			"spec": map[string]any{"type": "ExternalName", "externalName": "web." + f.Namespace + ".svc.cluster.local"}}
		f.Apply(ctx, framework.NewDeployment("web", 1, web, framework.DemoContainer()), svc, member, headless, alias)
		from := clientPod(ctx)
		hlPod := f.WaitForRunningPods(ctx, hl, 1)[0]
		s, err := client.Services(f.Client, f.Namespace).Get(ctx, "web")
		framework.ExpectNoError(err)
		ip := s.Spec.ClusterIP
		fqdn := "web." + f.Namespace + ".svc.cluster.local"

		By("resolving a Service by short, namespaced and fully qualified name")
		resolves(ctx, from.Name, ip+"\n", "web")
		resolves(ctx, from.Name, ip+"\n", "web."+f.Namespace)
		resolves(ctx, from.Name, ip+"\n", fqdn)
		resolves(ctx, from.Name, "10.96.0.1\n", "kubernetes.default")

		By("resolving a headless Service to its endpoints and its members by hostname")
		resolves(ctx, from.Name, hlPod.Status.PodIP+"\n", "hl")
		resolves(ctx, from.Name, hlPod.Status.PodIP+"\n", "member-0.hl")
		Expect(framework.KK(ctx, "get", "endpoints", "hl", "-n", f.Namespace, "-o", "yaml")).To(ContainSubstring("hostname: member-0"))

		By("answering SRV queries")
		resolves(ctx, from.Name, "0 100 80 "+fqdn+".\n", "-t", "SRV", "_http._tcp.web")
		resolves(ctx, from.Name, "0 100 8080 member-0.hl."+f.Namespace+".svc.cluster.local.\n", "-t", "SRV", "_http._tcp.hl")

		By("aliasing with an ExternalName and naming Pods by address")
		resolves(ctx, from.Name, fqdn+".\n", "-t", "CNAME", "alias")
		resolves(ctx, from.Name, ip+"\n", "alias")
		dashed := strings.ReplaceAll(from.Status.PodIP, ".", "-") + "." + f.Namespace + ".pod.cluster.local"
		resolves(ctx, from.Name, from.Status.PodIP+"\n", dashed)

		By("forwarding other names upstream and failing unknown cluster names")
		cp, err := client.Nodes(f.Client).Get(ctx, "control-plane")
		framework.ExpectNoError(err)
		resolves(ctx, from.Name, cp.InternalIP()+"\n", "control-plane")
		out, err := inPod(ctx, from.Name, "lookup", "nope."+f.Namespace+".svc.cluster.local")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("no such host"))
	})

	It("should configure resolv.conf by dnsPolicy and manage /etc/hosts", func(ctx SpecContext) {
		labels := map[string]string{"app": "conf"}
		named := framework.NewPod("named", labels, framework.MiniContainer("c", "sleep", "1h"))
		named.Spec()["hostname"], named.Spec()["subdomain"] = "db-0", "db"
		custom := framework.NewPod("custom", labels, framework.MiniContainer("c", "sleep", "1h"))
		custom.Spec()["dnsPolicy"] = "None"
		custom.Spec()["dnsConfig"] = map[string]any{"nameservers": []any{"10.96.0.10"}, "searches": []any{"example.test"},
			"options": []any{map[string]any{"name": "ndots", "value": "2"}}}
		node := framework.NewPod("node", labels, framework.MiniContainer("c", "sleep", "1h"))
		node.Spec()["dnsPolicy"] = "Default"
		f.Apply(ctx, named, custom, node)
		pods := f.WaitForRunningPods(ctx, labels, 3)
		byName := map[string]corev1.Pod{}
		for _, p := range pods {
			byName[p.Name] = p
		}

		Expect(inPod(ctx, "named", "cat", "/etc/resolv.conf")).To(And(
			ContainSubstring("search "+f.Namespace+".svc.cluster.local svc.cluster.local cluster.local"),
			ContainSubstring("nameserver 10.96.0.10\n"), ContainSubstring("options ndots:5")))
		Expect(inPod(ctx, "named", "cat", "/etc/hosts")).To(And(HavePrefix("# Kubernetes-managed hosts file.\n"),
			HaveSuffix(byName["named"].Status.PodIP+"\tdb-0.db."+f.Namespace+".svc.cluster.local\tdb-0\n")))
		Expect(inPod(ctx, "custom", "cat", "/etc/resolv.conf")).To(Equal("search example.test\nnameserver 10.96.0.10\noptions ndots:2\n"))
		host, err := client.Nodes(f.Client).Get(ctx, byName["node"].Spec.NodeName)
		framework.ExpectNoError(err)
		Expect(inPod(ctx, "node", "cat", "/etc/resolv.conf")).To(ContainSubstring("nameserver "+host.InternalIP()+"\n"),
			"dnsPolicy Default uses the node's resolver")
	})

	It("should give containers Service environment variables and reach the API through the kubernetes Service", func(ctx SpecContext) {
		f.Apply(ctx, framework.NewService("my-web", map[string]string{"app": "my-web"}, 8080))
		s, err := client.Services(f.Client, f.Namespace).Get(ctx, "my-web")
		framework.ExpectNoError(err)
		linked := map[string]string{"app": "linked"}
		unlinked := framework.NewPod("unlinked", linked, framework.MiniContainer("c", "sleep", "1h"))
		unlinked.Spec()["enableServiceLinks"] = false
		f.Apply(ctx, framework.NewPod("links", linked, framework.MiniContainer("c", "sleep", "1h")), unlinked)
		f.WaitForRunningPods(ctx, linked, 2)

		env, err := inPod(ctx, "links", "cat", "/proc/self/environ")
		framework.ExpectNoError(err)
		vars := strings.Split(env, "\x00")
		Expect(vars).To(ContainElements("MY_WEB_SERVICE_HOST="+s.Spec.ClusterIP, "MY_WEB_SERVICE_PORT=80",
			"MY_WEB_PORT=tcp://"+s.Spec.ClusterIP+":80", "MY_WEB_PORT_80_TCP_ADDR="+s.Spec.ClusterIP,
			"KUBERNETES_SERVICE_HOST=10.96.0.1", "KUBERNETES_SERVICE_PORT=443", "KUBERNETES_SERVICE_PORT_HTTPS=443"))
		env, err = inPod(ctx, "unlinked", "cat", "/proc/self/environ")
		framework.ExpectNoError(err)
		Expect(env).NotTo(ContainSubstring("MY_WEB_"))
		Expect(env).To(ContainSubstring("KUBERNETES_SERVICE_HOST=10.96.0.1"))

		Eventually(ctx, func() (string, error) {
			return inPod(ctx, "links", "fetch", "http://kubernetes.default.svc:443/healthz")
		}).WithTimeout(30 * time.Second).Should(Equal("200\nok"))
	})

	It("should keep answering when a DNS Pod goes away", Serial, func(ctx SpecContext) {
		from := clientPod(ctx)
		resolves(ctx, from.Name, "10.96.0.1\n", "kubernetes.default")
		dnsPods, err := client.Pods(f.Client, "kube-system").List(ctx, client.ListOptions{LabelSelector: "k8s-app=kube-dns"})
		framework.ExpectNoError(err)
		Expect(dnsPods.Items).To(HaveLen(2))
		framework.KK(ctx, "delete", "pod", "-n", "kube-system", dnsPods.Items[0].Name, "--wait=false")
		for range 10 {
			Expect(inPod(ctx, from.Name, "lookup", "kubernetes.default")).To(Equal("10.96.0.1\n"))
			time.Sleep(200 * time.Millisecond)
		}
		Eventually(ctx, func() string { return framework.KK(ctx, "get", "deployment", "kube-dns", "-n", "kube-system") }).
			WithTimeout(framework.TestContext.PodStartTimeout).Should(MatchRegexp(`kube-dns\s+2/2\s+2\s+2`))
	})
})
