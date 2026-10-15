package controlplane

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/controller/cronjob"
	"mini-k8s/pkg/controller/garbagecollector"
	"mini-k8s/pkg/controller/job"
	"mini-k8s/pkg/controller/ttlafterfinished"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/test/integration/framework"
)

// fakeClock is a settable clock for the CronJob controller.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) set(t time.Time) {
	f.mu.Lock()
	f.t = t
	f.mu.Unlock()
}

// startBatchControllers runs the Job, CronJob, TTL and garbage collection
// controllers. Nothing schedules or runs the Pods: tests finish them by
// writing their status.
func startBatchControllers(t *testing.T, c *client.Client) (context.Context, *fakeClock) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rec := record.New(c, "test", "")
	go rec.Run(ctx)
	pods := informers.New(client.Pods(c, ""), client.ListOptions{}, 0)
	jobs := informers.New(client.Jobs(c, ""), client.ListOptions{}, 0)
	cronJobs := informers.New(client.CronJobs(c, ""), client.ListOptions{}, 0)
	clock := &fakeClock{t: time.Now()}
	cjc := cronjob.NewController(c, cronJobs, jobs, rec)
	cjc.SetClock(clock.now)
	jc := job.NewController(c, jobs, pods, rec)
	ttl := ttlafterfinished.NewController(c, jobs)
	for _, run := range []func(context.Context){pods.Run, jobs.Run, cronJobs.Run} {
		go run(ctx)
	}
	informers.WaitForSync(ctx, pods.HasSynced, jobs.HasSynced, cronJobs.HasSynced)
	go jc.Run(ctx, 2)
	go cjc.Run(ctx, 1)
	go ttl.Run(ctx, 1)
	go garbagecollector.NewGarbageCollector(c, garbagecollector.TrackedResources()).Run(ctx)
	return ctx, clock
}

func newJob(name string, completions, parallelism *int32) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: batchv1.JobSpec{
		Completions: completions, Parallelism: parallelism,
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}}}}
}

// jobPods lists a Job's Pods that are not being deleted.
func jobPods(ctx context.Context, c *client.Client, name string) []corev1.Pod {
	list, err := client.Pods(c, "default").List(ctx, client.ListOptions{LabelSelector: batchv1.JobNameLabel + "=" + name})
	if err != nil {
		return nil
	}
	var out []corev1.Pod
	for _, p := range list.Items {
		if p.DeletionTimestamp == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func activePods(ctx context.Context, c *client.Client, name string) []corev1.Pod {
	var out []corev1.Pod
	for _, p := range jobPods(ctx, c, name) {
		if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			out = append(out, p)
		}
	}
	return out
}

// finishPod ends a Pod as a kubelet would; failures are dated an hour ago
// so the Job controller's backoff does not delay replacements.
func finishPod(t *testing.T, ctx context.Context, c *client.Client, p corev1.Pod, phase string) {
	t.Helper()
	code := int32(0)
	if phase == corev1.PodFailed {
		code = 1
	}
	p.Status.Phase = phase
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "c", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: code, FinishedAt: metav1.Time{Time: time.Now().Add(-time.Hour)}}}}}
	if _, err := client.Pods(c, p.Namespace).UpdateStatus(ctx, &p); err != nil {
		t.Fatal(err)
	}
}

func jobCondition(ctx context.Context, c *client.Client, name, typ string) *batchv1.JobCondition {
	j, err := client.Jobs(c, "default").Get(ctx, name)
	if err != nil {
		return nil
	}
	if cond := j.Status.Condition(typ); cond != nil && cond.Status == corev1.ConditionTrue {
		return cond
	}
	return nil
}

func TestJobRunsPodsToCompletion(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, _ := startBatchControllers(t, c)
	created, err := client.Jobs(c, "default").Create(ctx, newJob("work", ptr.To[int32](3), ptr.To[int32](2)))
	if err != nil {
		t.Fatal(err)
	}
	uid := created.UID
	if created.Spec.Selector.MatchLabels[batchv1.ControllerUIDLabel] != uid || created.Labels[batchv1.JobNameLabel] != "work" ||
		*created.Spec.BackoffLimit != 6 || *created.Spec.CompletionMode != batchv1.NonIndexedCompletion {
		t.Fatalf("defaults: %+v", created)
	}
	waitFor(t, "two Pods (the parallelism)", func() bool { return len(activePods(ctx, c, "work")) == 2 })
	pods := activePods(ctx, c, "work")
	for _, l := range []string{batchv1.ControllerUIDLabel, batchv1.LegacyControllerUIDLabel} {
		if pods[0].Labels[l] != uid {
			t.Fatalf("Pod labels: %v", pods[0].Labels)
		}
	}
	finishPod(t, ctx, c, pods[0], corev1.PodSucceeded)
	waitFor(t, "a replacement for the finished Pod", func() bool {
		j, _ := client.Jobs(c, "default").Get(ctx, "work")
		return len(activePods(ctx, c, "work")) == 2 && j.Status.Succeeded == 1 && j.Status.Active == 2
	})
	for _, p := range activePods(ctx, c, "work") {
		finishPod(t, ctx, c, p, corev1.PodSucceeded)
	}
	waitFor(t, "the Job to complete", func() bool { return jobCondition(ctx, c, "work", batchv1.JobComplete) != nil })
	j, _ := client.Jobs(c, "default").Get(ctx, "work")
	if j.Status.Succeeded != 3 || j.Status.Active != 0 || j.Status.CompletionTime == nil || j.Status.StartTime == nil ||
		j.Status.Condition(batchv1.JobSuccessCriteriaMet) == nil {
		t.Fatalf("status: %+v", j.Status)
	}
	if c := j.Status.Condition(batchv1.JobComplete); c.Reason != "CompletionsReached" || c.Message != "Reached expected number of succeeded pods" {
		t.Fatalf("condition: %+v", c)
	}
	waitFor(t, "the events", func() bool {
		return hasEvent(ctx, c, "work", "Completed", "Job completed") && hasEvent(ctx, c, "work", "SuccessfulCreate", "")
	})
	if n := len(jobPods(ctx, c, "work")); n != 3 {
		t.Fatalf("%d Pods, want the 3 that succeeded", n)
	}
}

func TestWorkQueueJobLetsRunningPodsFinish(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, _ := startBatchControllers(t, c)
	if _, err := client.Jobs(c, "default").Create(ctx, newJob("queue", nil, ptr.To[int32](3))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "three workers", func() bool { return len(activePods(ctx, c, "queue")) == 3 })
	pods := activePods(ctx, c, "queue")
	finishPod(t, ctx, c, pods[0], corev1.PodSucceeded)
	waitFor(t, "the success to be counted", func() bool {
		j, _ := client.Jobs(c, "default").Get(ctx, "queue")
		return j.Status.Succeeded == 1
	})
	time.Sleep(500 * time.Millisecond)
	if running := activePods(ctx, c, "queue"); len(running) != 2 {
		t.Fatalf("%d workers run, want the other 2 left to finish", len(running))
	}
	if jobCondition(ctx, c, "queue", batchv1.JobComplete) != nil {
		t.Fatal("complete while workers still run")
	}
	for _, p := range activePods(ctx, c, "queue") {
		finishPod(t, ctx, c, p, corev1.PodSucceeded)
	}
	waitFor(t, "the Job to complete", func() bool { return jobCondition(ctx, c, "queue", batchv1.JobComplete) != nil })
	if j, _ := client.Jobs(c, "default").Get(ctx, "queue"); j.Status.Succeeded != 3 || len(jobPods(ctx, c, "queue")) != 3 {
		t.Fatalf("status: %+v", j.Status)
	}
}

func TestJobFailsPastItsBackoffLimit(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, _ := startBatchControllers(t, c)
	j := newJob("flaky", nil, nil)
	j.Spec.BackoffLimit = ptr.To[int32](1)
	if _, err := client.Jobs(c, "default").Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		waitFor(t, "a running Pod", func() bool { return len(activePods(ctx, c, "flaky")) == 1 })
		finishPod(t, ctx, c, activePods(ctx, c, "flaky")[0], corev1.PodFailed)
	}
	waitFor(t, "the Job to fail", func() bool { return jobCondition(ctx, c, "flaky", batchv1.JobFailed) != nil })
	got, _ := client.Jobs(c, "default").Get(ctx, "flaky")
	if cond := got.Status.Condition(batchv1.JobFailed); cond.Reason != "BackoffLimitExceeded" || got.Status.Failed != 2 ||
		got.Status.Condition(batchv1.JobFailureTarget) == nil || got.Status.CompletionTime != nil {
		t.Fatalf("status: %+v", got.Status)
	}
	if n := len(activePods(ctx, c, "flaky")); n != 0 {
		t.Fatalf("%d Pods still run", n)
	}
	waitFor(t, "the warning", func() bool {
		return hasEvent(ctx, c, "flaky", "BackoffLimitExceeded", "Job has reached the specified backoff limit")
	})
}

func TestIndexedJobRunsEachIndexOnce(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, _ := startBatchControllers(t, c)
	j := newJob("idx", ptr.To[int32](3), ptr.To[int32](3))
	j.Spec.CompletionMode = ptr.To(batchv1.IndexedCompletion)
	if _, err := client.Jobs(c, "default").Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	indexes := func() map[string]corev1.Pod {
		out := map[string]corev1.Pod{}
		for _, p := range activePods(ctx, c, "idx") {
			out[p.Labels[batchv1.JobCompletionIndexKey]] = p
		}
		return out
	}
	waitFor(t, "a Pod per index", func() bool { return len(indexes()) == 3 })
	p1 := indexes()["1"]
	if p1.Spec.Hostname != "idx-1" || p1.Annotations[batchv1.JobCompletionIndexKey] != "1" ||
		p1.Spec.Containers[0].Env[0].Name != "JOB_COMPLETION_INDEX" {
		t.Fatalf("Pod of index 1: %+v", p1)
	}
	finishPod(t, ctx, c, p1, corev1.PodSucceeded)
	finishPod(t, ctx, c, indexes()["0"], corev1.PodFailed)
	waitFor(t, "index 0 to run again", func() bool {
		got, _ := client.Jobs(c, "default").Get(ctx, "idx")
		idx := indexes()
		_, has0 := idx["0"]
		return got.Status.CompletedIndexes == "1" && has0 && len(idx) == 2
	})
	for _, p := range activePods(ctx, c, "idx") {
		finishPod(t, ctx, c, p, corev1.PodSucceeded)
	}
	waitFor(t, "the Job to complete", func() bool { return jobCondition(ctx, c, "idx", batchv1.JobComplete) != nil })
	got, _ := client.Jobs(c, "default").Get(ctx, "idx")
	if got.Status.CompletedIndexes != "0-2" || got.Status.Succeeded != 3 || got.Status.Failed != 1 {
		t.Fatalf("status: %+v", got.Status)
	}
}

func TestSuspendedJobAndDeadline(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, _ := startBatchControllers(t, c)
	j := newJob("later", ptr.To[int32](2), ptr.To[int32](2))
	j.Spec.Suspend = ptr.To(true)
	if _, err := client.Jobs(c, "default").Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the Suspended condition", func() bool { return jobCondition(ctx, c, "later", batchv1.JobSuspended) != nil })
	if n := len(jobPods(ctx, c, "later")); n != 0 {
		t.Fatalf("a suspended Job runs %d Pods", n)
	}
	got, _ := client.Jobs(c, "default").Get(ctx, "later")
	if got.Status.StartTime != nil {
		t.Fatal("a Job created suspended has not started")
	}
	got.Spec.Suspend = ptr.To(false)
	got.Spec.ActiveDeadlineSeconds = ptr.To[int64](2)
	if _, err := client.Jobs(c, "default").Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the resumed Job's Pods", func() bool { return len(activePods(ctx, c, "later")) == 2 })
	if cond := jobCondition(ctx, c, "later", batchv1.JobSuspended); cond != nil {
		t.Fatalf("still suspended: %+v", cond)
	}
	waitFor(t, "the deadline", func() bool { return jobCondition(ctx, c, "later", batchv1.JobFailed) != nil })
	got, _ = client.Jobs(c, "default").Get(ctx, "later")
	if cond := got.Status.Condition(batchv1.JobFailed); cond.Reason != "DeadlineExceeded" {
		t.Fatalf("condition: %+v", cond)
	}
	waitFor(t, "the Pods to be deleted", func() bool { return len(jobPods(ctx, c, "later")) == 0 })
}

func TestFinishedJobIsDeletedAfterItsTTL(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, _ := startBatchControllers(t, c)
	j := newJob("short-lived", nil, nil)
	j.Spec.TTLSecondsAfterFinished = ptr.To[int32](1)
	if _, err := client.Jobs(c, "default").Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the Pod", func() bool { return len(activePods(ctx, c, "short-lived")) == 1 })
	finishPod(t, ctx, c, activePods(ctx, c, "short-lived")[0], corev1.PodSucceeded)
	waitFor(t, "the Job to complete", func() bool { return jobCondition(ctx, c, "short-lived", batchv1.JobComplete) != nil })
	waitFor(t, "the Job and its Pod to be deleted", func() bool {
		_, err := client.Jobs(c, "default").Get(ctx, "short-lived")
		list, _ := client.Pods(c, "default").List(ctx, client.ListOptions{})
		return client.IsNotFound(err) && list != nil && len(list.Items) == 0
	})
}

func TestCronJobSchedulesAndAppliesConcurrencyPolicies(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, clock := startBatchControllers(t, c)
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "tick"}, Spec: batchv1.CronJobSpec{
		Schedule: "*/1 * * * *", ConcurrencyPolicy: batchv1.ForbidConcurrent,
		JobTemplate: batchv1.JobTemplateSpec{Spec: newJob("", nil, nil).Spec}}}
	created, err := client.CronJobs(c, "default").Create(ctx, cj)
	if err != nil {
		t.Fatal(err)
	}
	if *created.Spec.SuccessfulJobsHistoryLimit != 3 || *created.Spec.FailedJobsHistoryLimit != 1 || *created.Spec.Suspend {
		t.Fatalf("defaults: %+v", created.Spec)
	}
	minute := created.CreationTimestamp.Truncate(time.Minute)
	// tickTo moves the clock past the n-th minute after the creation and
	// makes the controller look.
	tickTo := func(n int) time.Time {
		at := minute.Add(time.Duration(n) * time.Minute)
		clock.set(at.Add(time.Second))
		got, err := client.CronJobs(c, "default").Get(ctx, "tick")
		if err != nil {
			t.Fatal(err)
		}
		if got.Annotations == nil {
			got.Annotations = map[string]string{}
		}
		got.Annotations["tick"] = fmt.Sprint(n)
		if _, err := client.CronJobs(c, "default").Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		return at
	}
	status := func() batchv1.CronJobStatus {
		got, _ := client.CronJobs(c, "default").Get(ctx, "tick")
		return got.Status
	}
	first := tickTo(1)
	firstName := fmt.Sprintf("tick-%d", first.Unix()/60)
	waitFor(t, "the first Job", func() bool {
		s := status()
		return len(s.Active) == 1 && s.Active[0].Name == firstName && s.LastScheduleTime != nil && s.LastScheduleTime.Equal(first)
	})
	j, err := client.Jobs(c, "default").Get(ctx, firstName)
	if err != nil {
		t.Fatal(err)
	}
	if j.Annotations[batchv1.CronJobScheduledAnno] != first.UTC().Format(time.RFC3339) {
		t.Fatalf("annotations: %v", j.Annotations)
	}
	if ref := metav1.ControllerOf(&j.ObjectMeta); ref == nil || ref.Kind != "CronJob" || ref.Name != "tick" {
		t.Fatalf("owner: %+v", j.OwnerReferences)
	}

	// Forbid: the next run is skipped while the first is active.
	tickTo(2)
	waitFor(t, "the skipped run", func() bool {
		return hasEvent(ctx, c, "tick", "JobAlreadyActive", "Not starting job because prior execution is running and concurrency policy is Forbid")
	})
	if s := status(); len(s.Active) != 1 || !s.LastScheduleTime.Equal(first) {
		t.Fatalf("status after a forbidden run: %+v", s)
	}

	// Replace: the active Job makes way for the new one.
	got, _ := client.CronJobs(c, "default").Get(ctx, "tick")
	got.Spec.ConcurrencyPolicy = batchv1.ReplaceConcurrent
	if _, err := client.CronJobs(c, "default").Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	third := tickTo(3)
	thirdName := fmt.Sprintf("tick-%d", third.Unix()/60)
	waitFor(t, "the replacement", func() bool {
		s := status()
		_, err := client.Jobs(c, "default").Get(ctx, firstName)
		return len(s.Active) == 1 && s.Active[0].Name == thirdName && client.IsNotFound(err)
	})

	// The finished Job leaves the active list and sets lastSuccessfulTime.
	waitFor(t, "the Job's Pod", func() bool { return len(activePods(ctx, c, thirdName)) == 1 })
	finishPod(t, ctx, c, activePods(ctx, c, thirdName)[0], corev1.PodSucceeded)
	waitFor(t, "the completed Job to be seen", func() bool {
		s := status()
		return len(s.Active) == 0 && s.LastSuccessfulTime != nil &&
			hasEvent(ctx, c, "tick", "SawCompletedJob", "Saw completed job: "+thirdName+", condition: Complete")
	})

	// Allow, and the history keeps the newest successful Job only.
	got, _ = client.CronJobs(c, "default").Get(ctx, "tick")
	got.Spec.ConcurrencyPolicy, got.Spec.SuccessfulJobsHistoryLimit = batchv1.AllowConcurrent, ptr.To[int32](1)
	if _, err := client.CronJobs(c, "default").Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	for n := 4; n <= 5; n++ {
		at := tickTo(n)
		name := fmt.Sprintf("tick-%d", at.Unix()/60)
		waitFor(t, "Job "+name, func() bool { return len(activePods(ctx, c, name)) == 1 })
		finishPod(t, ctx, c, activePods(ctx, c, name)[0], corev1.PodSucceeded)
		waitFor(t, "Job "+name+" to complete", func() bool { return jobCondition(ctx, c, name, batchv1.JobComplete) != nil })
	}
	waitFor(t, "the history to be trimmed", func() bool {
		list, _ := client.Jobs(c, "default").List(ctx, client.ListOptions{})
		return list != nil && len(list.Items) == 1 && list.Items[0].Name == fmt.Sprintf("tick-%d", minute.Add(5*time.Minute).Unix()/60)
	})

	// A missed run past the starting deadline is not started.
	got, _ = client.CronJobs(c, "default").Get(ctx, "tick")
	got.Spec.StartingDeadlineSeconds = ptr.To[int64](10)
	if _, err := client.CronJobs(c, "default").Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	clock.set(minute.Add(6*time.Minute + 30*time.Second))
	got, _ = client.CronJobs(c, "default").Get(ctx, "tick")
	got.Annotations["tick"] = "late"
	if _, err := client.CronJobs(c, "default").Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if s := status(); len(s.Active) != 0 || !s.LastScheduleTime.Equal(minute.Add(5*time.Minute)) {
		t.Fatalf("a run past its deadline started: %+v", s)
	}
}

func TestSuspendedCronJobRunsNothing(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, clock := startBatchControllers(t, c)
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "paused"}, Spec: batchv1.CronJobSpec{
		Schedule: "*/1 * * * *", Suspend: ptr.To(true), JobTemplate: batchv1.JobTemplateSpec{Spec: newJob("", nil, nil).Spec}}}
	if _, err := client.CronJobs(c, "default").Create(ctx, cj); err != nil {
		t.Fatal(err)
	}
	clock.set(time.Now().Add(5 * time.Minute))
	got, _ := client.CronJobs(c, "default").Get(ctx, "paused")
	got.Labels = map[string]string{"poke": "1"}
	if _, err := client.CronJobs(c, "default").Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if list, _ := client.Jobs(c, "default").List(ctx, client.ListOptions{}); len(list.Items) != 0 {
		t.Fatalf("a suspended CronJob created %d Jobs", len(list.Items))
	}
	got, _ = client.CronJobs(c, "default").Get(ctx, "paused")
	got.Spec.Suspend = ptr.To(false)
	if _, err := client.CronJobs(c, "default").Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a Job after resuming", func() bool {
		list, _ := client.Jobs(c, "default").List(ctx, client.ListOptions{})
		return list != nil && len(list.Items) == 1
	})
}
