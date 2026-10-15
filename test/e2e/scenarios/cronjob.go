package scenarios

import (
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("scenarios", "Scheduled work", func() {
	f := framework.NewDefaultFramework("overlap")

	// cronJob runs a Job every minute whose Pod never finishes by itself.
	cronJob := func(policy string) framework.Object {
		c := framework.ContainerObject(framework.MiniContainer("work", "sleep", "1h"))
		return framework.Object{"apiVersion": "batch/v1", "kind": "CronJob", "metadata": map[string]any{"name": "report"},
			"spec": map[string]any{"schedule": "*/1 * * * *", "concurrencyPolicy": policy,
				"jobTemplate": map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
					"restartPolicy": "Never", "terminationGracePeriodSeconds": 1, "containers": []any{c}}}}}}}
	}
	// jobs lists the names of the Jobs that are not being deleted.
	jobs := func(ctx SpecContext) []string {
		list, err := client.Jobs(f.Client, f.Namespace).List(ctx, client.ListOptions{})
		framework.ExpectNoError(err)
		var out []string
		for _, j := range list.Items {
			if j.DeletionTimestamp == nil {
				out = append(out, j.Name)
			}
		}
		sort.Strings(out)
		return out
	}
	active := func(ctx SpecContext) []string {
		cj, err := client.CronJobs(f.Client, f.Namespace).Get(ctx, "report")
		framework.ExpectNoError(err)
		var out []string
		for _, r := range cj.Status.Active {
			out = append(out, r.Name)
		}
		return out
	}

	It("should skip runs while one is active under Forbid and replace the active run under Replace", framework.WithSlow(), func(ctx SpecContext) {
		f.Apply(ctx, cronJob("Forbid"))

		By("starting the first run at the next minute")
		Eventually(ctx, func() []string { return jobs(ctx) }).WithTimeout(90 * time.Second).WithPolling(time.Second).Should(HaveLen(1))
		first := jobs(ctx)[0]
		Eventually(ctx, func() []string { return active(ctx) }).WithTimeout(30 * time.Second).Should(Equal([]string{first}))
		f.WaitForRunningPods(ctx, map[string]string{batchv1.JobNameLabel: first}, 1)

		By("skipping the next minute's run while the first one is still active")
		Eventually(ctx, func() []string {
			var reasons []string
			for _, e := range f.Events(ctx, "report") {
				reasons = append(reasons, e.Reason)
			}
			return reasons
		}).WithTimeout(90 * time.Second).WithPolling(time.Second).Should(ContainElement("JobAlreadyActive"))
		Expect(jobs(ctx)).To(Equal([]string{first}))
		Expect(f.Events(ctx, "report")).To(ContainElement(HaveField("Message",
			"Not starting job because prior execution is running and concurrency policy is Forbid")))

		By("switching to Replace: the next run takes the place of the active one")
		f.Apply(ctx, cronJob("Replace"))
		Eventually(ctx, func() []string { return jobs(ctx) }).WithTimeout(90 * time.Second).WithPolling(time.Second).
			Should(And(HaveLen(1), Not(ContainElement(first))))
		second := jobs(ctx)[0]
		Expect(second > first).To(BeTrue(), "%s is a later run than %s", second, first)
		Expect(f.Events(ctx, "report")).To(ContainElement(HaveField("Message", "Deleted job "+first)))
		Eventually(ctx, func() []string { return active(ctx) }).WithTimeout(30 * time.Second).Should(Equal([]string{second}))
		Eventually(ctx, func() ([]string, error) {
			pods, err := f.Pods(ctx, map[string]string{batchv1.JobNameLabel: first})
			var names []string
			for _, p := range pods {
				names = append(names, p.Name)
			}
			return names, err
		}).WithTimeout(time.Minute).Should(BeEmpty(), "the replaced run's Pod goes with it")
		f.WaitForRunningPods(ctx, map[string]string{batchv1.JobNameLabel: second}, 1)
		Expect(f.KK(ctx, "get", "cronjob", "report")).To(MatchRegexp(`(?m)^report\s+\*/1 \* \* \* \*\s+<none>\s+False\s+1\s+`))
	})
})
