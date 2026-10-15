package validation

import (
	"strings"
	"testing"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/deepcopy"
	"mini-k8s/pkg/util/ptr"
)

func validJob() *batchv1.Job {
	labels := map[string]string{batchv1.ControllerUIDLabel: "u"}
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j"}, Spec: batchv1.JobSpec{
		Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}}}}
	batchv1.SetDefaultsJobSpec(&j.Spec)
	return j
}

func expectError(t *testing.T, errs []string, want string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e, want) {
			return
		}
	}
	t.Errorf("errors %q lack %q", errs, want)
}

func TestValidateJob(t *testing.T) {
	if errs := ValidateJob(validJob()); len(errs) != 0 {
		t.Fatalf("valid Job: %v", errs)
	}
	j := validJob()
	j.Spec.Completions = ptr.To[int32](-1)
	expectError(t, ValidateJob(j), "spec.completions: Invalid value: -1: must be greater than or equal to 0")
	j = validJob()
	j.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
	expectError(t, ValidateJob(j), `spec.template.spec.restartPolicy: Required value: valid values: "OnFailure", "Never"`)
	j = validJob()
	j.Spec.CompletionMode, j.Spec.Completions = ptr.To(batchv1.IndexedCompletion), nil
	expectError(t, ValidateJob(j), "spec.completions: Required value: when completion mode is Indexed")
	j = validJob()
	j.Spec.CompletionMode = ptr.To("Sometimes")
	expectError(t, ValidateJob(j), `spec.completionMode: Unsupported value: "Sometimes"`)
	j = validJob()
	j.Spec.Template.Labels = map[string]string{"other": "x"}
	expectError(t, ValidateJob(j), "selector does not match template labels")
}

func TestValidateJobUpdate(t *testing.T) {
	old := validJob()
	j := validJob()
	j.Spec.Parallelism, j.Spec.Suspend, j.Spec.ActiveDeadlineSeconds = ptr.To[int32](3), ptr.To(true), ptr.To[int64](60)
	if errs := ValidateJobUpdate(j, old); len(errs) != 0 {
		t.Fatalf("mutable fields: %v", errs)
	}
	j.Spec.Completions = ptr.To[int32](5)
	j.Spec.Template.Spec.Containers[0].Image = "nginx"
	errs := ValidateJobUpdate(j, old)
	expectError(t, errs, "spec.completions: Invalid value: 5: field is immutable")
	expectError(t, errs, "spec.template: Invalid value: field is immutable")
}

func TestValidateCronJob(t *testing.T) {
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "c"}, Spec: batchv1.CronJobSpec{Schedule: "*/1 * * * *",
		JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyOnFailure, Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}}}}}}
	batchv1.SetDefaultsCronJob(cj)
	if errs := ValidateCronJob(cj); len(errs) != 0 {
		t.Fatalf("valid CronJob: %v", errs)
	}
	for _, tc := range []struct {
		change func(*batchv1.CronJob)
		want   string
	}{
		{func(c *batchv1.CronJob) { c.Spec.Schedule = "every minute" },
			`spec.schedule: Invalid value: "every minute": expected exactly 5 fields, found 2: [every minute]`},
		{func(c *batchv1.CronJob) { c.Spec.Schedule = "" }, "spec.schedule: Required value"},
		{func(c *batchv1.CronJob) { c.Spec.ConcurrencyPolicy = "Sometimes" },
			`spec.concurrencyPolicy: Unsupported value: "Sometimes": supported values: "Allow", "Forbid", "Replace"`},
		{func(c *batchv1.CronJob) { c.Spec.TimeZone = ptr.To("Mars/Olympus") }, `spec.timeZone: Invalid value: "Mars/Olympus"`},
		{func(c *batchv1.CronJob) { c.Spec.TimeZone = ptr.To("Local") }, `spec.timeZone: Invalid value: "Local"`},
		{func(c *batchv1.CronJob) { c.Name = strings.Repeat("a", 53) }, "must be no more than 52 characters"},
		{func(c *batchv1.CronJob) { c.Spec.StartingDeadlineSeconds = ptr.To[int64](-1) }, "spec.startingDeadlineSeconds: Invalid value: -1"},
		{func(c *batchv1.CronJob) {
			c.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
		},
			"spec.jobTemplate.spec.template.spec.restartPolicy: Required value"},
	} {
		c := deepcopy.Copy(cj)
		tc.change(c)
		expectError(t, ValidateCronJob(c), tc.want)
	}
	cj.Spec.TimeZone = ptr.To("Asia/Shanghai")
	if errs := ValidateCronJob(cj); len(errs) != 0 {
		t.Fatalf("a valid time zone: %v", errs)
	}
}
