package scenarios

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("scenarios", "Safe rollouts", func() {
	f := framework.NewDefaultFramework("saferollout")

	It("should keep serving from the old Pods while a broken rollout stalls, and keep them through kk rollout undo", func(ctx SpecContext) {
		host := f.Namespace + ".example.com"
		get := func(ctx context.Context) (int, string, error) {
			return framework.HTTPGetHost(ctx, framework.TestContext.IngressURL+"/", host)
		}
		labels := map[string]string{"app": "shop"}
		version := func(v string, broken bool) framework.Object {
			c := framework.DemoContainer()
			c.Env = map[string]string{"VERSION": v, "DEMO_SHUTDOWN_DELAY": "2s"}
			if broken {
				c.Env["DEMO_START_DELAY"] = "1h" // runs, but never listens and so never gets ready
			}
			c.Fields = map[string]any{"readinessProbe": readiness}
			d := framework.NewDeployment("shop", 3, labels, c)
			d.Spec()["progressDeadlineSeconds"] = 10
			d.PodSpec()["terminationGracePeriodSeconds"] = 5 // the broken version ignores SIGTERM while it waits
			d.Spec()["strategy"] = map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": 1, "maxUnavailable": 0}}
			return d
		}
		f.Apply(ctx, version("1", false), framework.NewService("shop", labels, 8080),
			framework.NewIngress("shop", host, framework.IngressPath("/", "Prefix", "shop", 80)))
		f.KK(ctx, "rollout", "status", "deployment/shop", "--timeout", "90s")
		stable := f.WaitForRunningPods(ctx, labels, 3)
		var answers, uids []string
		for _, p := range stable {
			answers = append(answers, "hello from "+p.Name+"\n")
			uids = append(uids, p.UID)
		}
		Eventually(ctx, func() (int, error) {
			code, _, err := get(ctx)
			return code, err
		}).WithTimeout(20 * time.Second).Should(Equal(200))

		By("rolling out a version that never gets ready: the rollout stalls past its deadline")
		var status string
		bodies := whileServing(ctx, get, func() {
			f.Apply(ctx, version("2", true))
			var err error
			status, err = framework.RunKK(ctx, "rollout", "status", "deployment/shop", "-n", f.Namespace, "--timeout", "60s")
			Expect(err).To(HaveOccurred())
		})
		Expect(status).To(ContainSubstring(`deployment "shop" exceeded its progress deadline`))
		Expect(bodies).NotTo(BeEmpty())
		for _, b := range bodies {
			Expect(b).To(BeElementOf(answers), "only the old Pods answer")
		}
		Expect(f.KK(ctx, "get", "deployment", "shop")).To(MatchRegexp(`(?m)^shop\s+3/3\s+1\s+3\s+`), "one new Pod, not ready")
		d, err := client.Deployments(f.Client, f.Namespace).Get(ctx, "shop")
		framework.ExpectNoError(err)
		Expect(d.Status.Condition(appsv1.DeploymentProgressing)).To(And(HaveField("Status", "False"), HaveField("Reason", "ProgressDeadlineExceeded")))
		pods, err := f.Pods(ctx, labels)
		framework.ExpectNoError(err)
		Expect(pods).To(HaveLen(4))
		notReady := 0
		for i := range pods {
			if !corev1.IsPodReady(&pods[i]) {
				notReady++
			}
		}
		Expect(notReady).To(Equal(1))

		By("undoing the rollout: the broken Pod goes and the old Pods stay")
		bodies = whileServing(ctx, get, func() {
			Expect(f.KK(ctx, "rollout", "undo", "deployment/shop")).To(Equal("deployment.apps/shop rolled back\n"))
			f.KK(ctx, "rollout", "status", "deployment/shop", "--timeout", "60s")
		})
		for _, b := range bodies {
			Expect(b).To(BeElementOf(answers))
		}
		var after []string
		for _, p := range f.WaitForRunningPods(ctx, labels, 3) {
			after = append(after, p.UID)
		}
		Expect(after).To(ConsistOf(uids), "no old Pod was replaced")
		Expect(f.KK(ctx, "get", "deployment", "shop")).To(MatchRegexp(`(?m)^shop\s+3/3\s+3\s+3\s+`))
		Expect(f.KK(ctx, "rollout", "history", "deployment/shop")).To(MatchRegexp(`(?s)\n2\s+<none>\n3\s+<none>\n`),
			"the restored template becomes revision 3")
	})
})
