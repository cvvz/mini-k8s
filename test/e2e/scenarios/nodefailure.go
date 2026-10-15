package scenarios

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("scenarios", "Node failure", func() {
	f := framework.NewDefaultFramework("nodefailure")

	It("should move stateless Pods off a lost node but keep its StatefulSet Pod and local volume until the node returns",
		framework.WithDisruptive(), Serial, func(ctx SpecContext) {
			// Evict 5s after the node becomes unreachable instead of 300s.
			tolerations := []any{map[string]any{"key": corev1.TaintNodeUnreachable, "operator": "Exists",
				"effect": "NoExecute", "tolerationSeconds": 5}}
			apiLabels, dbLabels := map[string]string{"app": "api"}, map[string]string{"app": "db"}
			web := framework.DemoContainer()
			web.Fields = map[string]any{"readinessProbe": readiness}
			api := framework.NewDeployment("api", 2, apiLabels, web)
			api.PodSpec()["tolerations"] = tolerations
			db := newStatefulSet("db", 1, web)
			db.PodSpec()["tolerations"] = tolerations
			onNode := func(pods []corev1.Pod, node string) int {
				n := 0
				for _, p := range pods {
					if p.Spec.NodeName == node {
						n++
					}
				}
				return n
			}
			nodeReady := func(ctx SpecContext) string {
				n, err := client.Nodes(f.Client).Get(ctx, "worker-2")
				framework.ExpectNoError(err)
				return n.Status.Condition(corev1.NodeReady).Status
			}

			By("running the API and the store on worker-2, and a client on worker-1")
			framework.KK(ctx, "cordon", "worker-1")
			DeferCleanup(func(ctx SpecContext) { framework.KK(ctx, "uncordon", "worker-1") })
			f.Apply(ctx, api, framework.NewService("api", apiLabels, 8080), headlessService("db"), db)
			lost := f.WaitForRunningPods(ctx, apiLabels, 2)
			oldDB := f.WaitForRunningPods(ctx, dbLabels, 1)[0]
			framework.KK(ctx, "uncordon", "worker-1")
			Expect(onNode(append(lost, oldDB), "worker-2")).To(Equal(3))
			clientPod := framework.NewPod("client", map[string]string{"app": "client"}, framework.MiniContainer("app", "sleep", "1h"))
			clientPod.Spec()["nodeSelector"] = map[string]any{"kubernetes.io/hostname": "worker-1"}
			f.Apply(ctx, clientPod)
			f.WaitForRunningPods(ctx, map[string]string{"app": "client"}, 1)
			_, err := mini(ctx, f.Namespace, "db-0", "write", "/data/state", "v1")
			framework.ExpectNoError(err)
			Eventually(ctx, func() (string, error) { return f.FetchFromPod(ctx, "client", "http://api/") }).
				WithTimeout(30 * time.Second).Should(HavePrefix("200\nhello from api-"))

			By("losing worker-2")
			_, err = framework.Docker(ctx, "stop", framework.ComponentContainer("worker-2"))
			framework.ExpectNoError(err)
			DeferCleanup(func(ctx SpecContext) {
				_, _ = framework.Docker(ctx, "start", framework.ComponentContainer("worker-2"))
				framework.WaitForClusterReady(ctx, len(framework.Nodes))
			})
			Eventually(ctx, nodeReady).WithTimeout(time.Minute).WithPolling(time.Second).Should(Equal(corev1.ConditionUnknown))

			By("replacing the API Pods on worker-1 while the Service keeps answering")
			moved := f.WaitForRunningPods(ctx, apiLabels, 2)
			Expect(onNode(moved, "worker-1")).To(Equal(2))
			Eventually(ctx, func() (string, error) { return f.FetchFromPod(ctx, "client", "http://api/") }).
				WithTimeout(30 * time.Second).Should(HavePrefix("200\nhello from api-"))
			for range 5 {
				out, err := f.FetchFromPod(ctx, "client", "http://api/")
				framework.ExpectNoError(err)
				Expect(out).To(Or(Equal("200\nhello from "+moved[0].Name+"\n"), Equal("200\nhello from "+moved[1].Name+"\n")))
			}

			By("evicting db-0 without replacing it: it may still run on the unreachable node")
			Eventually(ctx, func() (bool, error) {
				p, err := client.Pods(f.Client, f.Namespace).Get(ctx, "db-0")
				return err == nil && p.DeletionTimestamp != nil, err
			}).WithTimeout(time.Minute).Should(BeTrue())
			Consistently(ctx, func() (string, error) {
				p, err := client.Pods(f.Client, f.Namespace).Get(ctx, "db-0")
				if err != nil {
					return "", err
				}
				return p.UID, nil
			}).WithTimeout(5*time.Second).Should(Equal(oldDB.UID), "at most one db-0")

			By("force-deleting db-0: its replacement waits for the node that holds its volume")
			framework.KK(ctx, "delete", "pod", "db-0", "-n", f.Namespace, "--force", "--grace-period=0")
			Eventually(ctx, func(g Gomega) {
				p, err := client.Pods(f.Client, f.Namespace).Get(ctx, "db-0")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(p.UID).NotTo(Equal(oldDB.UID))
				g.Expect(p.Spec.NodeName).To(BeEmpty())
				c := p.Status.Condition(corev1.PodScheduled)
				g.Expect(c).NotTo(BeNil())
				g.Expect(c.Reason).To(Equal("Unschedulable"))
				g.Expect(c.Message).To(ContainSubstring("volume node affinity conflict"))
			}).WithTimeout(time.Minute).Should(Succeed())

			By("bringing worker-2 back: db-0 starts there with its data, and the API spreads onto it again")
			_, err = framework.Docker(ctx, "start", framework.ComponentContainer("worker-2"))
			framework.ExpectNoError(err)
			Eventually(ctx, nodeReady).WithTimeout(2 * time.Minute).WithPolling(time.Second).Should(Equal(corev1.ConditionTrue))
			replaced := waitForReplacement(ctx, f, &oldDB)
			Expect(replaced.Spec.NodeName).To(Equal("worker-2"))
			Expect(mini(ctx, f.Namespace, "db-0", "cat", "/data/state")).To(Equal("v1\n"))
			for i := range lost {
				f.WaitForPodGone(ctx, &lost[i], 2*time.Minute)
			}
			f.KK(ctx, "scale", "deployment", "api", "--replicas", "4")
			Expect(onNode(f.WaitForRunningPods(ctx, apiLabels, 4), "worker-2")).To(Equal(2))
			Eventually(ctx, func() (int, error) {
				ep, err := client.Endpoints(f.Client, f.Namespace).Get(ctx, "api")
				if err != nil || len(ep.Subsets) == 0 {
					return 0, err
				}
				return len(ep.Subsets[0].Addresses), nil
			}).WithTimeout(30*time.Second).Should(Equal(4), "every API Pod serves the Service")
		})
})
