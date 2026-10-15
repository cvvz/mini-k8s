// Package validation checks batch/v1 objects like Kubernetes.
package validation

import (
	"fmt"
	"reflect"
	"time"
	_ "time/tzdata" // CronJob time zones without the host's database

	appsvalidation "mini-k8s/pkg/apis/apps/validation"
	batchv1 "mini-k8s/pkg/apis/batch/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	corevalidation "mini-k8s/pkg/apis/core/validation"
	"mini-k8s/pkg/util/cron"
)

// maxCronJobNameLength leaves room for the suffix of the Jobs it creates.
const maxCronJobNameLength = 52

func nonNegative32(path string, v *int32) []string {
	if v != nil && *v < 0 {
		return []string{fmt.Sprintf("%s: Invalid value: %d: must be greater than or equal to 0", path, *v)}
	}
	return nil
}

func nonNegative64(path string, v *int64) []string {
	if v != nil && *v < 0 {
		return []string{fmt.Sprintf("%s: Invalid value: %d: must be greater than or equal to 0", path, *v)}
	}
	return nil
}

// validateJobSpec checks a Job's spec; a CronJob's template has no
// selector yet (each Job gets its own).
func validateJobSpec(path string, s *batchv1.JobSpec, withSelector bool) []string {
	var errs []string
	errs = append(errs, nonNegative32(path+".parallelism", s.Parallelism)...)
	errs = append(errs, nonNegative32(path+".completions", s.Completions)...)
	errs = append(errs, nonNegative32(path+".backoffLimit", s.BackoffLimit)...)
	errs = append(errs, nonNegative64(path+".activeDeadlineSeconds", s.ActiveDeadlineSeconds)...)
	errs = append(errs, nonNegative32(path+".ttlSecondsAfterFinished", s.TTLSecondsAfterFinished)...)
	if m := s.CompletionMode; m != nil {
		switch *m {
		case batchv1.NonIndexedCompletion:
		case batchv1.IndexedCompletion:
			if s.Completions == nil {
				errs = append(errs, path+".completions: Required value: when completion mode is Indexed")
			}
		default:
			errs = append(errs, fmt.Sprintf(`%s.completionMode: Unsupported value: %q: supported values: "NonIndexed", "Indexed"`, path, *m))
		}
	}
	if withSelector {
		errs = append(errs, appsvalidation.ValidateSelectorMatchesTemplate(path, s.Selector, &s.Template)...)
	}
	errs = append(errs, corevalidation.ValidatePodSpec(path+".template.spec", &s.Template.Spec)...)
	if p := s.Template.Spec.RestartPolicy; p != corev1.RestartPolicyOnFailure && p != corev1.RestartPolicyNever {
		errs = append(errs, path+`.template.spec.restartPolicy: Required value: valid values: "OnFailure", "Never"`)
	}
	return errs
}

// ValidateJob validates a complete Job.
func ValidateJob(j *batchv1.Job) []string { return validateJobSpec("spec", &j.Spec, true) }

// ValidateJobUpdate keeps what defines the Pods a Job runs.
func ValidateJobUpdate(j, old *batchv1.Job) []string {
	var errs []string
	if !reflect.DeepEqual(j.Spec.Completions, old.Spec.Completions) && j.Spec.Completions != nil {
		errs = append(errs, fmt.Sprintf("spec.completions: Invalid value: %d: field is immutable", *j.Spec.Completions))
	}
	if !reflect.DeepEqual(j.Spec.Selector, old.Spec.Selector) {
		errs = append(errs, "spec.selector: Invalid value: field is immutable")
	}
	if !reflect.DeepEqual(j.Spec.Template, old.Spec.Template) {
		errs = append(errs, "spec.template: Invalid value: field is immutable")
	}
	if !reflect.DeepEqual(j.Spec.CompletionMode, old.Spec.CompletionMode) {
		errs = append(errs, "spec.completionMode: Invalid value: field is immutable")
	}
	return errs
}

// ValidateCronJob validates a complete CronJob.
func ValidateCronJob(cj *batchv1.CronJob) []string {
	var errs []string
	if len(cj.Name) > maxCronJobNameLength {
		errs = append(errs, fmt.Sprintf("metadata.name: Invalid value: %q: must be no more than %d characters", cj.Name, maxCronJobNameLength))
	}
	if cj.Spec.Schedule == "" {
		errs = append(errs, "spec.schedule: Required value")
	} else if _, err := cron.Parse(cj.Spec.Schedule); err != nil {
		errs = append(errs, fmt.Sprintf("spec.schedule: Invalid value: %q: %v", cj.Spec.Schedule, err))
	}
	if tz := cj.Spec.TimeZone; tz != nil {
		if _, err := time.LoadLocation(*tz); err != nil || *tz == "" || *tz == "Local" {
			errs = append(errs, fmt.Sprintf("spec.timeZone: Invalid value: %q: unknown time zone %s", *tz, *tz))
		}
	}
	switch cj.Spec.ConcurrencyPolicy {
	case batchv1.AllowConcurrent, batchv1.ForbidConcurrent, batchv1.ReplaceConcurrent:
	default:
		errs = append(errs, fmt.Sprintf(`spec.concurrencyPolicy: Unsupported value: %q: supported values: "Allow", "Forbid", "Replace"`, cj.Spec.ConcurrencyPolicy))
	}
	errs = append(errs, nonNegative64("spec.startingDeadlineSeconds", cj.Spec.StartingDeadlineSeconds)...)
	errs = append(errs, nonNegative32("spec.successfulJobsHistoryLimit", cj.Spec.SuccessfulJobsHistoryLimit)...)
	errs = append(errs, nonNegative32("spec.failedJobsHistoryLimit", cj.Spec.FailedJobsHistoryLimit)...)
	return append(errs, validateJobSpec("spec.jobTemplate.spec", &cj.Spec.JobTemplate.Spec, false)...)
}
