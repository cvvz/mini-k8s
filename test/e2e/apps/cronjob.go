package apps

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

// newCronJob builds a CronJob manifest whose Jobs run c once.
func newCronJob(name, schedule string, spec map[string]any, c framework.Container) framework.Object {
	spec["schedule"] = schedule
	spec["jobTemplate"] = map[string]any{"spec": newJob("", map[string]any{}, c).Spec()}
	return framework.Object{"apiVersion": "batch/v1", "kind": "CronJob", "metadata": map[string]any{"name": name}, "spec": spec}
}

var _ = framework.SIGDescribe("apps", "CronJob", func() {
	f := framework.NewDefaultFramework("cronjob")

	ownedJobs := func(ctx SpecContext, cj *batchv1.CronJob) []batchv1.Job {
		list, err := client.Jobs(f.Client, f.Namespace).List(ctx, client.ListOptions{})
		framework.ExpectNoError(err)
		var out []batchv1.Job
		for _, j := range list.Items {
			if ref := metav1.ControllerOf(&j.ObjectMeta); ref != nil && ref.UID == cj.UID {
				out = append(out, j)
			}
		}
		return out
	}

	It("should run a Job at each scheduled minute and track it", framework.WithSlow(), func(ctx SpecContext) {
		f.Apply(ctx, newCronJob("ticker", "*/1 * * * *", map[string]any{"concurrencyPolicy": "Forbid"},
			framework.MiniContainer("work", "sleep", "1s")))
		Expect(f.KK(ctx, "get", "cronjobs")).To(MatchRegexp(`(?m)^ticker\s+\*/1 \* \* \* \*\s+<none>\s+False\s+`))
		cj, err := client.CronJobs(f.Client, f.Namespace).Get(ctx, "ticker")
		framework.ExpectNoError(err)

		By("waiting for the next minute's Job")
		var job batchv1.Job
		Eventually(ctx, func() []batchv1.Job { return ownedJobs(ctx, cj) }).WithTimeout(90 * time.Second).
			WithPolling(time.Second).ShouldNot(BeEmpty())
		job = ownedJobs(ctx, cj)[0]
		scheduled, err := time.Parse(time.RFC3339, job.Annotations[batchv1.CronJobScheduledAnno])
		framework.ExpectNoError(err)
		Expect(scheduled.Second()).To(BeZero())
		Expect(job.Name).To(Equal(fmt.Sprintf("ticker-%d", scheduled.Unix()/60)))

		By("seeing the Job complete")
		Eventually(ctx, func(g Gomega) {
			got, err := client.CronJobs(f.Client, f.Namespace).Get(ctx, "ticker")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.Status.LastScheduleTime).NotTo(BeNil())
			g.Expect(got.Status.LastScheduleTime.Equal(scheduled)).To(BeTrue())
			g.Expect(got.Status.LastSuccessfulTime).NotTo(BeNil())
			g.Expect(got.Status.Active).To(BeEmpty())
		}).WithTimeout(90 * time.Second).Should(Succeed())
		Expect(f.Events(ctx, "ticker")).To(ContainElements(
			HaveField("Message", "Created job "+job.Name),
			HaveField("Message", "Saw completed job: "+job.Name+", condition: Complete")))
		Expect(f.KK(ctx, "get", "cronjob", "ticker")).To(MatchRegexp(`(?m)^ticker\s+\*/1 \* \* \* \*\s+<none>\s+False\s+0\s+\d+s\s+`))

		By("running it by hand with kk create job --from")
		Expect(f.KK(ctx, "create", "job", "by-hand", "--from=cronjob/ticker")).To(Equal("job.batch/by-hand created\n"))
		Eventually(ctx, func() string { return f.KK(ctx, "get", "job", "by-hand") }).WithTimeout(90 * time.Second).
			Should(MatchRegexp(`(?m)^by-hand\s+Complete\s+1/1\s+`))
		manual, err := client.Jobs(f.Client, f.Namespace).Get(ctx, "by-hand")
		framework.ExpectNoError(err)
		Expect(manual.Annotations).To(HaveKeyWithValue(batchv1.CronJobInstantiateAnno, "manual"))

		By("deleting the CronJob: its Jobs go with it")
		f.KK(ctx, "delete", "cronjob", "ticker")
		Eventually(ctx, func() []batchv1.Job { return ownedJobs(ctx, cj) }).WithTimeout(time.Minute).Should(BeEmpty())
	})

	It("should reject invalid schedules, time zones and policies", func(ctx SpecContext) {
		c := framework.MiniContainer("work", "sleep", "1s")
		Expect(f.ApplyExpectError(ctx, newCronJob("bad", "every minute", map[string]any{}, c))).
			To(ContainSubstring(`spec.schedule: Invalid value: "every minute": expected exactly 5 fields, found 2: [every minute]`))
		Expect(f.ApplyExpectError(ctx, newCronJob("bad", "@hourly", map[string]any{"timeZone": "Mars/Olympus"}, c))).
			To(ContainSubstring(`spec.timeZone: Invalid value: "Mars/Olympus"`))
		Expect(f.ApplyExpectError(ctx, newCronJob("bad", "@hourly", map[string]any{"concurrencyPolicy": "Sometimes"}, c))).
			To(ContainSubstring(`spec.concurrencyPolicy: Unsupported value: "Sometimes": supported values: "Allow", "Forbid", "Replace"`))

		f.Apply(ctx, newCronJob("paused", "0 3 * * *", map[string]any{"suspend": true, "timeZone": "Asia/Shanghai"}, c))
		Expect(f.KK(ctx, "get", "cj", "paused")).To(MatchRegexp(`(?m)^paused\s+0 3 \* \* \*\s+Asia/Shanghai\s+True\s+0\s+<none>\s+`))
		Expect(f.KK(ctx, "describe", "cronjob", "paused")).To(And(ContainSubstring("Concurrency Policy:"),
			ContainSubstring("Successful Job History Limit:")))
	})
})
