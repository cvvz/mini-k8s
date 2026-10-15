package apps

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/test/e2e/framework"
)

// newJob builds a Job manifest; spec holds the Job's own fields.
func newJob(name string, spec map[string]any, c framework.Container) framework.Object {
	spec["template"] = map[string]any{"spec": map[string]any{"restartPolicy": "Never", "terminationGracePeriodSeconds": 1,
		"containers": []any{framework.ContainerObject(c)}}}
	return framework.Object{"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]any{"name": name}, "spec": spec}
}

var _ = framework.SIGDescribe("apps", "Job", func() {
	f := framework.NewDefaultFramework("job")

	jobPods := func(ctx SpecContext, name string) []corev1.Pod {
		list, err := client.Pods(f.Client, f.Namespace).List(ctx, client.ListOptions{LabelSelector: batchv1.JobNameLabel + "=" + name})
		framework.ExpectNoError(err)
		return list.Items
	}
	getJob := func(ctx SpecContext, name string) *batchv1.Job {
		j, err := client.Jobs(f.Client, f.Namespace).Get(ctx, name)
		framework.ExpectNoError(err)
		return j
	}
	// waitFinished waits for the Job's Complete or Failed condition.
	waitFinished := func(ctx SpecContext, name, want string, timeout time.Duration) *batchv1.Job {
		var j *batchv1.Job
		Eventually(ctx, func() string {
			j = getJob(ctx, name)
			_, how := batchv1.IsFinished(j)
			return how
		}).WithTimeout(timeout).Should(Equal(want), "Job %s", name)
		return j
	}

	It("should run Pods in parallel until enough of them succeed", func(ctx SpecContext) {
		f.Apply(ctx, newJob("pi", map[string]any{"completions": 3, "parallelism": 2}, framework.MiniContainer("work", "sleep", "2s")))
		By("running at most two Pods at once")
		Consistently(ctx, func() int {
			running := 0
			for _, p := range jobPods(ctx, "pi") {
				if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed && p.DeletionTimestamp == nil {
					running++
				}
			}
			return running
		}).WithTimeout(3 * time.Second).Should(BeNumerically("<=", 2))
		j := waitFinished(ctx, "pi", batchv1.JobComplete, 90*time.Second)
		Expect(j.Status.Succeeded).To(BeEquivalentTo(3))
		Expect(j.Status.CompletionTime).NotTo(BeNil())
		Expect(j.Status.Condition(batchv1.JobComplete).Reason).To(Equal("CompletionsReached"))
		Expect(j.Labels).To(HaveKeyWithValue(batchv1.JobNameLabel, "pi"))

		pods := jobPods(ctx, "pi")
		Expect(pods).To(HaveLen(3))
		for _, p := range pods {
			Expect(p.Status.Phase).To(Equal(corev1.PodSucceeded))
			Expect(p.Labels).To(HaveKeyWithValue(batchv1.ControllerUIDLabel, j.UID))
		}
		Expect(f.KK(ctx, "get", "jobs")).To(MatchRegexp(`(?m)^pi\s+Complete\s+3/3\s+\d+s\s+`))
		Expect(f.KK(ctx, "get", "pods")).To(MatchRegexp(`(?m)^pi-\w+\s+0/1\s+Completed\s+0\s+`))
		Expect(f.KK(ctx, "describe", "job", "pi")).To(ContainSubstring("0 Active (0 Ready) / 3 Succeeded / 0 Failed"))
		Expect(f.Events(ctx, "pi")).To(ContainElements(HaveField("Reason", "SuccessfulCreate"), HaveField("Reason", "Completed")))
	})

	It("should run every index of an Indexed Job exactly once", func(ctx SpecContext) {
		f.Apply(ctx, newJob("idx", map[string]any{"completions": 3, "parallelism": 3, "completionMode": "Indexed"},
			framework.MiniContainer("work", "env")))
		j := waitFinished(ctx, "idx", batchv1.JobComplete, 90*time.Second)
		Expect(j.Status.CompletedIndexes).To(Equal("0-2"))
		Expect(f.KK(ctx, "get", "job", "idx")).To(MatchRegexp(`(?m)^idx\s+Complete\s+3/3\s+`))
		seen := map[string]bool{}
		for _, p := range jobPods(ctx, "idx") {
			index := p.Labels[batchv1.JobCompletionIndexKey]
			Expect(p.Name).To(HavePrefix("idx-" + index + "-"))
			Expect(p.Spec.Hostname).To(Equal("idx-" + index))
			Expect(f.KK(ctx, "logs", p.Name)).To(ContainSubstring("JOB_COMPLETION_INDEX=" + index + "\n"))
			seen[index] = true
		}
		Expect(seen).To(Equal(map[string]bool{"0": true, "1": true, "2": true}))
	})

	It("should fail a Job past its backoff limit or its active deadline", func(ctx SpecContext) {
		f.Apply(ctx,
			newJob("crash", map[string]any{"backoffLimit": 1}, framework.CrashingContainer()),
			newJob("slow", map[string]any{"activeDeadlineSeconds": 5}, framework.MiniContainer("work", "sleep", "1h")))

		By("replacing the first failed Pod once, then failing")
		j := waitFinished(ctx, "crash", batchv1.JobFailed, 2*time.Minute)
		Expect(j.Status.Failed).To(BeEquivalentTo(2))
		Expect(j.Status.Condition(batchv1.JobFailed).Reason).To(Equal("BackoffLimitExceeded"))
		Expect(jobPods(ctx, "crash")).To(HaveEach(HaveField("Status.Phase", corev1.PodFailed)))
		Expect(f.KK(ctx, "get", "job", "crash")).To(MatchRegexp(`(?m)^crash\s+Failed\s+0/1\s+`))
		Expect(f.Events(ctx, "crash")).To(ContainElement(And(HaveField("Reason", "BackoffLimitExceeded"),
			HaveField("Message", "Job has reached the specified backoff limit"))))

		By("stopping the slow Job's Pod at the deadline")
		j = waitFinished(ctx, "slow", batchv1.JobFailed, time.Minute)
		Expect(j.Status.Condition(batchv1.JobFailed).Reason).To(Equal("DeadlineExceeded"))
		Eventually(ctx, func() []corev1.Pod { return jobPods(ctx, "slow") }).WithTimeout(time.Minute).Should(BeEmpty())
	})

	It("should count container restarts of OnFailure Pods against the backoff limit", func(ctx SpecContext) {
		job := newJob("restarts", map[string]any{"backoffLimit": 1}, framework.CrashingContainer())
		job.Spec()["template"].(map[string]any)["spec"].(map[string]any)["restartPolicy"] = "OnFailure"
		f.Apply(ctx, job)
		j := waitFinished(ctx, "restarts", batchv1.JobFailed, 2*time.Minute)
		Expect(j.Status.Condition(batchv1.JobFailed).Reason).To(Equal("BackoffLimitExceeded"))
		Eventually(ctx, func() []corev1.Pod { return jobPods(ctx, "restarts") }).WithTimeout(time.Minute).Should(BeEmpty(),
			"the restarting Pod is deleted")
	})

	It("should hold a suspended Job's Pods until it is resumed", func(ctx SpecContext) {
		f.Apply(ctx, newJob("held", map[string]any{"suspend": true, "completions": 2, "parallelism": 2},
			framework.MiniContainer("work", "sleep", "1s")))
		Eventually(ctx, func() string { return f.KK(ctx, "get", "job", "held") }).Should(MatchRegexp(`(?m)^held\s+Suspended\s+0/2\s+`))
		Consistently(ctx, func() []corev1.Pod { return jobPods(ctx, "held") }).WithTimeout(3 * time.Second).Should(BeEmpty())

		By("resuming it")
		j := getJob(ctx, "held")
		j.Spec.Suspend = ptr.To(false)
		_, err := client.Jobs(f.Client, f.Namespace).Update(ctx, j)
		framework.ExpectNoError(err)
		j = waitFinished(ctx, "held", batchv1.JobComplete, 90*time.Second)
		Expect(j.Status.Condition(batchv1.JobSuspended)).To(HaveField("Status", corev1.ConditionFalse))
		Expect(f.Events(ctx, "held")).To(ContainElements(HaveField("Reason", "Suspended"), HaveField("Reason", "Resumed")))
	})

	It("should delete a finished Job and its Pods after the TTL", func(ctx SpecContext) {
		f.Apply(ctx, newJob("ephemeral", map[string]any{"ttlSecondsAfterFinished": 3}, framework.MiniContainer("work", "sleep", "1s")))
		waitFinished(ctx, "ephemeral", batchv1.JobComplete, 90*time.Second)
		Eventually(ctx, func() error {
			_, err := client.Jobs(f.Client, f.Namespace).Get(ctx, "ephemeral")
			return err
		}).WithTimeout(time.Minute).Should(MatchError(ContainSubstring("not found")))
		Eventually(ctx, func() []corev1.Pod { return jobPods(ctx, "ephemeral") }).WithTimeout(time.Minute).Should(BeEmpty())
	})

	It("should create a Job from the command line with kk create job", func(ctx SpecContext) {
		// -n goes before "--": everything after it is the container's command.
		Expect(framework.KK(ctx, "create", "job", "hello", "-n", f.Namespace, "--image", framework.DemoImage, "--", "/mini", "sleep", "1s")).
			To(Equal("job.batch/hello created\n"))
		Expect(getJob(ctx, "hello").Spec.Template.Spec.Containers[0].Command).To(Equal([]string{"/mini", "sleep", "1s"}))
		waitFinished(ctx, "hello", batchv1.JobComplete, 90*time.Second)
		out, err := framework.RunKK(ctx, "create", "job", "bad", "-n", f.Namespace)
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("either --image or --from must be specified"))
		out, err = framework.RunKK(ctx, "create", "job", "hello", "--image", framework.DemoImage, "-n", f.Namespace)
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring(fmt.Sprintf(`jobs.batch %q already exists`, "hello")))
	})

	It("should reject invalid Jobs and changes to immutable fields", func(ctx SpecContext) {
		out := f.ApplyExpectError(ctx, newJob("bad", map[string]any{"completions": -1}, framework.MiniContainer("work", "sleep", "1s")))
		Expect(out).To(ContainSubstring("spec.completions: Invalid value: -1: must be greater than or equal to 0"))
		always := newJob("always", map[string]any{}, framework.MiniContainer("work", "sleep", "1s"))
		always.Spec()["template"].(map[string]any)["spec"].(map[string]any)["restartPolicy"] = "Always"
		Expect(f.ApplyExpectError(ctx, always)).To(ContainSubstring(`spec.template.spec.restartPolicy: Required value: valid values: "OnFailure", "Never"`))

		f.Apply(ctx, newJob("fixed", map[string]any{"completions": 1, "suspend": true}, framework.MiniContainer("work", "sleep", "1s")))
		out = f.ApplyExpectError(ctx, newJob("fixed", map[string]any{"completions": 5, "suspend": true}, framework.MiniContainer("work", "sleep", "1s")))
		Expect(out).To(ContainSubstring("spec.completions: Invalid value: 5: field is immutable"))
	})
})
