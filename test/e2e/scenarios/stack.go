package scenarios

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("scenarios", "Application stack", func() {
	f := framework.NewDefaultFramework("stack")

	It("should serve a web tier through an Ingress, reach a stateful store by stable names and keep the store's data", framework.WithSlow(), func(ctx SpecContext) {
		host := f.Namespace + ".example.com"
		get := func(ctx context.Context) (int, string, error) {
			return framework.HTTPGetHost(ctx, framework.TestContext.IngressURL+"/", host)
		}
		config := func(greeting string) framework.Object {
			return framework.Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "web-config"},
				"data": map[string]any{"GREETING": greeting, "STORE_URL": "http://store-0.store:8080/"}}
		}
		web := framework.DemoContainer()
		web.Fields = map[string]any{"readinessProbe": readiness,
			"envFrom": []any{map[string]any{"configMapRef": map[string]any{"name": "web-config"}}},
			"env": []any{
				// Keep serving briefly after SIGTERM while the Ingress drops the Pod.
				map[string]any{"name": "DEMO_SHUTDOWN_DELAY", "value": "2s"},
				map[string]any{"name": "STORE_PASSWORD", "valueFrom": map[string]any{
					"secretKeyRef": map[string]any{"name": "store-creds", "key": "password"}}},
			}}
		webTier := framework.NewDeployment("web", 2, map[string]string{"app": "web"}, web)
		webTier.Spec()["strategy"] = map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": 1, "maxUnavailable": 0}}
		store := framework.DemoContainer()
		store.Fields = map[string]any{"readinessProbe": readiness}

		By("creating the configuration, a store of two replicas with a volume each, and a web tier behind an Ingress")
		f.Apply(ctx, config("hi"),
			framework.Object{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "store-creds"},
				"stringData": map[string]any{"password": "s3cret"}},
			headlessService("store"), newStatefulSet("store", 2, store),
			webTier, framework.NewService("web", map[string]string{"app": "web"}, 8080),
			framework.NewIngress("web", host, framework.IngressPath("/", "Prefix", "web", 80)))
		f.KK(ctx, "rollout", "status", "statefulset/store", "--timeout", "120s")
		f.KK(ctx, "rollout", "status", "deployment/web", "--timeout", "90s")
		webPods := f.WaitForRunningPods(ctx, map[string]string{"app": "web"}, 2)

		By("balancing Ingress traffic over both web Pods")
		seen := map[string]bool{}
		Eventually(ctx, func(g Gomega) {
			code, body, err := get(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(code).To(Equal(200))
			seen[strings.TrimSpace(strings.TrimPrefix(body, "hello from "))] = true
			g.Expect(seen).To(And(HaveKey(webPods[0].Name), HaveKey(webPods[1].Name)))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		By("giving the web tier its configuration and the store's password")
		env, err := mini(ctx, f.Namespace, webPods[0].Name, "env")
		framework.ExpectNoError(err)
		Expect(strings.Split(env, "\n")).To(ContainElements("GREETING=hi", "STORE_URL=http://store-0.store:8080/", "STORE_PASSWORD=s3cret"))

		By("reaching every store replica by its stable DNS name")
		for _, name := range []string{"store-0", "store-1"} {
			Eventually(ctx, func() (string, error) { return f.FetchFromPod(ctx, webPods[0].Name, "http://"+name+".store:8080/") }).
				WithTimeout(30 * time.Second).Should(Equal("200\nhello from " + name + "\n"))
		}

		By("keeping store-0's data on its volume when the Pod is replaced")
		_, err = mini(ctx, f.Namespace, "store-0", "write", "/data/state", "v1")
		framework.ExpectNoError(err)
		old, err := client.Pods(f.Client, f.Namespace).Get(ctx, "store-0")
		framework.ExpectNoError(err)
		f.KK(ctx, "delete", "pod", "store-0")
		replaced := waitForReplacement(ctx, f, old)
		Expect(replaced.Spec.NodeName).To(Equal(old.Spec.NodeName), "its local volume pins it to the node")
		Expect(mini(ctx, f.Namespace, "store-0", "cat", "/data/state")).To(Equal("v1\n"))
		Eventually(ctx, func() (string, error) { return mini(ctx, f.Namespace, webPods[0].Name, "lookup", "store-0.store") }).
			WithTimeout(30 * time.Second).Should(Equal(replaced.Status.PodIP + "\n"))

		By("rolling a configuration change out to the web tier without failing a request")
		f.Apply(ctx, config("hello"))
		bodies := whileServing(ctx, get, func() {
			f.KK(ctx, "rollout", "restart", "deployment/web")
			f.KK(ctx, "rollout", "status", "deployment/web", "--timeout", "90s")
		})
		Expect(len(bodies)).To(BeNumerically(">", 5))
		restarted := f.WaitForRunningPods(ctx, map[string]string{"app": "web"}, 2)
		for _, p := range restarted {
			Expect(p.UID).NotTo(BeElementOf(webPods[0].UID, webPods[1].UID))
			env, err := mini(ctx, f.Namespace, p.Name, "env")
			framework.ExpectNoError(err)
			Expect(strings.Split(env, "\n")).To(ContainElement("GREETING=hello"))
		}

		By("deleting the namespace, which removes the workloads, their containers and the provisioned volumes")
		claims, err := client.PersistentVolumeClaims(f.Client, f.Namespace).List(ctx, client.ListOptions{})
		framework.ExpectNoError(err)
		Expect(claims.Items).To(HaveLen(2))
		pods, err := client.Pods(f.Client, f.Namespace).List(ctx, client.ListOptions{})
		framework.ExpectNoError(err)
		f.DeleteNamespace(ctx, f.Namespace)
		Eventually(ctx, func(g Gomega) {
			for _, p := range pods.Items {
				ids, err := framework.PodContainers(ctx, p.UID, false)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ids).To(BeEmpty(), "containers of Pod %s", p.Name)
			}
			for _, c := range claims.Items {
				_, err := client.PersistentVolumes(f.Client).Get(ctx, c.Spec.VolumeName)
				g.Expect(client.IsNotFound(err)).To(BeTrue(), "volume %s of claim %s: %v", c.Spec.VolumeName, c.Name, err)
			}
		}).WithTimeout(2 * time.Minute).WithPolling(time.Second).Should(Succeed())
		// Another Ingress, such as the README demo's catch-all, may answer
		// the host now; the deleted web Pods must not.
		Eventually(ctx, func() (string, error) {
			_, body, err := get(ctx)
			return body, err
		}).WithTimeout(30*time.Second).Should(And(Not(ContainSubstring(restarted[0].Name)), Not(ContainSubstring(restarted[1].Name))),
			"the Ingress is gone too")
	})
})
