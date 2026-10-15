package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/util/ptr"
)

const createJobUsage = "usage: kk create job NAME --image IMAGE [-- COMMAND [ARG...]] | kk create job NAME --from=cronjob/NAME"

// jobStatus is kubectl's STATUS column of a Job.
func jobStatus(j *batchv1.Job) string {
	has := func(t string) bool {
		c := j.Status.Condition(t)
		return c != nil && c.Status == corev1.ConditionTrue
	}
	switch {
	case has(batchv1.JobComplete):
		return "Complete"
	case has(batchv1.JobFailed):
		return "Failed"
	case j.DeletionTimestamp != nil:
		return "Terminating"
	case has(batchv1.JobSuspended):
		return "Suspended"
	case has(batchv1.JobFailureTarget):
		return "FailureTarget"
	case has(batchv1.JobSuccessCriteriaMet):
		return "SuccessCriteriaMet"
	}
	return "Running"
}

// jobCompletions is kubectl's COMPLETIONS column: "1/3", or "0/1 of 2"
// for a work queue running several Pods.
func jobCompletions(j *batchv1.Job) string {
	switch {
	case j.Spec.Completions != nil:
		return fmt.Sprintf("%d/%d", j.Status.Succeeded, *j.Spec.Completions)
	case j.Spec.Parallelism != nil && *j.Spec.Parallelism > 1:
		return fmt.Sprintf("%d/1 of %d", j.Status.Succeeded, *j.Spec.Parallelism)
	}
	return fmt.Sprintf("%d/1", j.Status.Succeeded)
}

// jobDuration is how long the Job ran (so far).
func jobDuration(j *batchv1.Job, now time.Time) string {
	switch {
	case j.Status.StartTime == nil:
		return ""
	case j.Status.CompletionTime == nil:
		return humanDuration(now.Sub(j.Status.StartTime.Time))
	}
	return humanDuration(j.Status.CompletionTime.Sub(j.Status.StartTime.Time))
}

func formatSelector(s *metav1.LabelSelector) string {
	if s == nil {
		return "<none>"
	}
	sel, err := s.AsSelector()
	if err != nil {
		return "<error>"
	}
	return orNone(sel.String())
}

func boolPtr(b *bool) string {
	switch {
	case b == nil:
		return "<unset>"
	case *b:
		return "True"
	}
	return "False"
}

func int32Ptr(v *int32) string {
	if v == nil {
		return "<unset>"
	}
	return fmt.Sprint(*v)
}

// createJob implements kk create job: from an image and command, or as a
// manual run of a CronJob's template, like kubectl create job.
func (a *app) createJob(ctx context.Context, p args) error {
	if len(p.positional) != 2 {
		return errors.New(createJobUsage)
	}
	name, image, from := p.positional[1], p.value("image", ""), p.value("from", "")
	var job *batchv1.Job
	switch {
	case image == "" && from == "":
		return errors.New("either --image or --from must be specified")
	case image != "" && from != "":
		return errors.New("--image and --from are mutually exclusive")
	case from != "" && len(p.rest) > 0:
		return errors.New("cannot specify --from and command")
	case image != "":
		job = &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.namespace}, Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers:    []corev1.Container{{Name: name, Image: image, Command: p.rest}},
				RestartPolicy: corev1.RestartPolicyNever,
			}}}}
	default:
		typ, cjName, ok := strings.Cut(from, "/")
		if !ok || cjName == "" || (typ != "cronjob" && typ != "cronjobs" && typ != "cj" && typ != "cronjob.batch") {
			return fmt.Errorf("--from must be cronjob/NAME, not %q", from)
		}
		cj, err := client.CronJobs(a.client, a.namespace).Get(ctx, cjName)
		if err != nil {
			return err
		}
		annotations := map[string]string{batchv1.CronJobInstantiateAnno: "manual"}
		for k, v := range cj.Spec.JobTemplate.Annotations {
			annotations[k] = v
		}
		job = &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.namespace, Annotations: annotations, Labels: cj.Spec.JobTemplate.Labels,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "CronJob", Name: cj.Name, UID: cj.UID, Controller: ptr.To(true)}}},
			Spec: cj.Spec.JobTemplate.Spec,
		}
	}
	if _, err := client.Jobs(a.client, a.namespace).Create(ctx, job); err != nil {
		return createError("job", err)
	}
	_, err := fmt.Fprintf(a.out, "job.batch/%s created\n", name)
	return err
}
