package main

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/test/integration/framework"
)

func TestJobColumns(t *testing.T) {
	now := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	start := metav1.Time{Time: now.Add(-90 * time.Second)}
	cond := func(typ string) []batchv1.JobCondition {
		return []batchv1.JobCondition{{Type: typ, Status: corev1.ConditionTrue}}
	}
	for name, tc := range map[string]struct {
		job                              batchv1.Job
		status, completions, durationCol string
	}{
		"running": {batchv1.Job{Spec: batchv1.JobSpec{Completions: ptr.To[int32](3)},
			Status: batchv1.JobStatus{StartTime: &start, Succeeded: 1}}, "Running", "1/3", "90s"},
		"complete": {batchv1.Job{Spec: batchv1.JobSpec{Completions: ptr.To[int32](1)}, Status: batchv1.JobStatus{
			StartTime: &start, CompletionTime: &metav1.Time{Time: start.Add(5 * time.Second)}, Succeeded: 1,
			Conditions: cond(batchv1.JobComplete)}}, "Complete", "1/1", "5s"},
		"work queue": {batchv1.Job{Spec: batchv1.JobSpec{Parallelism: ptr.To[int32](2)},
			Status: batchv1.JobStatus{Conditions: cond(batchv1.JobFailed)}}, "Failed", "0/1 of 2", ""},
		"suspended": {batchv1.Job{Spec: batchv1.JobSpec{Parallelism: ptr.To[int32](1)},
			Status: batchv1.JobStatus{Conditions: cond(batchv1.JobSuspended)}}, "Suspended", "0/1", ""},
		"terminating": {batchv1.Job{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &start}}, "Terminating", "0/1", ""},
	} {
		if s, c, d := jobStatus(&tc.job), jobCompletions(&tc.job), jobDuration(&tc.job, now); s != tc.status ||
			c != tc.completions || d != tc.durationCol {
			t.Errorf("%s: %q %q %q, want %q %q %q", name, s, c, d, tc.status, tc.completions, tc.durationCol)
		}
	}
}

func TestCreateJobAndBatchTables(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := context.Background()
	if out, err := kk(t, c, "", "create", "job", "hello", "--image", "busybox", "--", "echo", "hi"); err != nil ||
		out != "job.batch/hello created\n" {
		t.Fatalf("create job: %q %v", out, err)
	}
	j, err := client.Jobs(c, "default").Get(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	ct := j.Spec.Template.Spec.Containers[0]
	if ct.Name != "hello" || ct.Image != "busybox" || strings.Join(ct.Command, " ") != "echo hi" ||
		j.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("job spec: %+v", j.Spec.Template.Spec)
	}
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"create", "job", "x"}, "either --image or --from must be specified"},
		{[]string{"create", "job", "x", "--image", "a", "--from", "cronjob/y"}, "--image and --from are mutually exclusive"},
		{[]string{"create", "job", "x", "--from", "cronjob/y", "--", "sh"}, "cannot specify --from and command"},
		{[]string{"create", "job", "x", "--from", "deployment/y"}, `--from must be cronjob/NAME, not "deployment/y"`},
		{[]string{"create", "job", "x", "--from", "cronjob/missing"}, "not found"},
		{[]string{"create", "job", "hello", "--image", "busybox"}, `failed to create job: jobs.batch "hello" already exists`},
	} {
		if _, err := kk(t, c, "", tc.argv...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.argv, err, tc.want)
		}
	}

	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly"}, Spec: batchv1.CronJobSpec{
		Schedule: "0 3 * * *", TimeZone: ptr.To("Asia/Shanghai"),
		JobTemplate: batchv1.JobTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"team": "data"}},
			Spec: batchv1.JobSpec{Template: j.Spec.Template}}}}
	cj.Spec.JobTemplate.Spec.Template.Labels = nil
	created, err := client.CronJobs(c, "default").Create(ctx, cj)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := kk(t, c, "", "create", "job", "manual", "--from=cronjob/nightly"); err != nil || out != "job.batch/manual created\n" {
		t.Fatalf("create job --from: %q %v", out, err)
	}
	m, err := client.Jobs(c, "default").Get(ctx, "manual")
	if err != nil {
		t.Fatal(err)
	}
	ref := metav1.ControllerOf(&m.ObjectMeta)
	if m.Annotations[batchv1.CronJobInstantiateAnno] != "manual" || m.Labels["team"] != "data" || ref == nil ||
		ref.Kind != "CronJob" || ref.UID != created.UID {
		t.Fatalf("manual job: %+v", m.ObjectMeta)
	}

	out, err := kk(t, c, "", "get", "jobs")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || strings.Join(strings.Fields(lines[0]), " ") != "NAME STATUS COMPLETIONS DURATION AGE" ||
		!strings.HasPrefix(strings.Join(strings.Fields(lines[1]), " "), "hello Running 0/1") {
		t.Fatalf("kk get jobs:\n%s", out)
	}
	out, err = kk(t, c, "", "get", "cj")
	if err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "NAME SCHEDULE TIMEZONE SUSPEND ACTIVE LAST SCHEDULE AGE" ||
		!strings.HasPrefix(strings.Join(strings.Fields(lines[1]), " "), "nightly 0 3 * * * Asia/Shanghai False 0 <none>") {
		t.Fatalf("kk get cronjobs:\n%s", out)
	}
	out, err = kk(t, c, "", "describe", "cronjob", "nightly")
	if err != nil || !strings.Contains(out, "Concurrency Policy:") || !strings.Contains(out, "Allow") ||
		!strings.Contains(out, "Successful Job History Limit:") {
		t.Fatalf("describe cronjob: %v\n%s", err, out)
	}
	out, err = kk(t, c, "", "describe", "job", "hello")
	if err != nil || !strings.Contains(out, "Pods Statuses:") || !strings.Contains(out, "Completion Mode:") {
		t.Fatalf("describe job: %v\n%s", err, out)
	}
}
