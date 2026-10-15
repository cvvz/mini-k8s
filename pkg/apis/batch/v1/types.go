// Package v1 contains the batch/v1 types (batchv1): Job, which runs Pods
// to completion, and CronJob, which creates Jobs on a schedule.
package v1

import (
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/ptr"
)

// Job runs Pods until enough of them succeed.
type Job struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              JobSpec   `json:"spec"`
	Status            JobStatus `json:"status,omitzero"`
}

// JobSpec is the desired run.
type JobSpec struct {
	// Parallelism is the most Pods running at once (1 by default).
	Parallelism *int32 `json:"parallelism,omitempty"`
	// Completions is the number of successful Pods needed; nil means any
	// success completes the Job once no Pod runs (a work queue).
	Completions *int32 `json:"completions,omitempty"`
	// ActiveDeadlineSeconds fails the Job when it runs longer.
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`
	// BackoffLimit is the number of failures tolerated (6 by default).
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`
	// Selector is generated from the Job's UID unless ManualSelector.
	Selector       *metav1.LabelSelector  `json:"selector,omitempty"`
	ManualSelector *bool                  `json:"manualSelector,omitempty"`
	Template       corev1.PodTemplateSpec `json:"template"`
	// TTLSecondsAfterFinished deletes the Job that long after it finished.
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
	// CompletionMode is NonIndexed (default) or Indexed: each index from 0
	// to completions-1 needs one successful Pod.
	CompletionMode *string `json:"completionMode,omitempty"`
	// Suspend stops the Job's Pods until it is resumed.
	Suspend *bool `json:"suspend,omitempty"`
	// PodReplacementPolicy is shown as Kubernetes defaults it.
	PodReplacementPolicy *string `json:"podReplacementPolicy,omitempty"`
}

// Completion modes.
const (
	NonIndexedCompletion = "NonIndexed"
	IndexedCompletion    = "Indexed"
)

// JobStatus is maintained by the Job controller.
type JobStatus struct {
	Conditions     []JobCondition `json:"conditions,omitempty"`
	StartTime      *metav1.Time   `json:"startTime,omitempty"`
	CompletionTime *metav1.Time   `json:"completionTime,omitempty"`
	Active         int32          `json:"active,omitempty"`
	Succeeded      int32          `json:"succeeded,omitempty"`
	Failed         int32          `json:"failed,omitempty"`
	Terminating    *int32         `json:"terminating,omitempty"`
	Ready          *int32         `json:"ready,omitempty"`
	// CompletedIndexes lists the succeeded indexes of an Indexed Job, as
	// ranges: "0-2,5".
	CompletedIndexes string `json:"completedIndexes,omitempty"`
	// UncountedTerminatedPods is always empty: Pods are counted as they
	// are (legacy tracking).
	UncountedTerminatedPods *UncountedTerminatedPods `json:"uncountedTerminatedPods,omitempty"`
}

// UncountedTerminatedPods lists Pods whose end is not yet counted.
type UncountedTerminatedPods struct {
	Succeeded []string `json:"succeeded,omitempty"`
	Failed    []string `json:"failed,omitempty"`
}

// Job condition types.
const (
	JobComplete           = "Complete"
	JobFailed             = "Failed"
	JobSuspended          = "Suspended"
	JobFailureTarget      = "FailureTarget"
	JobSuccessCriteriaMet = "SuccessCriteriaMet"
)

// JobCondition is one aspect of a Job's state.
type JobCondition struct {
	Type               string      `json:"type"`
	Status             string      `json:"status"`
	LastProbeTime      metav1.Time `json:"lastProbeTime,omitzero"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitzero"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
}

// Condition returns the condition of the given type, if present.
func (s *JobStatus) Condition(t string) *JobCondition {
	for i := range s.Conditions {
		if s.Conditions[i].Type == t {
			return &s.Conditions[i]
		}
	}
	return nil
}

// IsFinished reports whether the Job completed or failed, and how.
func IsFinished(j *Job) (bool, string) {
	for _, c := range j.Status.Conditions {
		if (c.Type == JobComplete || c.Type == JobFailed) && c.Status == corev1.ConditionTrue {
			return true, c.Type
		}
	}
	return false, ""
}

// Labels, annotations and environment of Jobs and their Pods.
const (
	ControllerUIDLabel       = "batch.kubernetes.io/controller-uid"
	LegacyControllerUIDLabel = "controller-uid"
	JobNameLabel             = "batch.kubernetes.io/job-name"
	LegacyJobNameLabel       = "job-name"
	JobCompletionIndexKey    = "batch.kubernetes.io/job-completion-index"
	JobCompletionIndexEnv    = "JOB_COMPLETION_INDEX"
	CronJobScheduledAnno     = "batch.kubernetes.io/cronjob-scheduled-timestamp"
	CronJobInstantiateAnno   = "cronjob.kubernetes.io/instantiate"
)

// CronJob creates Jobs from a template on a cron schedule.
type CronJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              CronJobSpec   `json:"spec"`
	Status            CronJobStatus `json:"status,omitzero"`
}

// CronJobSpec is the schedule and the Job template.
type CronJobSpec struct {
	// Schedule is a cron expression: "*/5 * * * *", "@hourly".
	Schedule string `json:"schedule"`
	// TimeZone of the schedule (the controller's, UTC, by default).
	TimeZone *string `json:"timeZone,omitempty"`
	// StartingDeadlineSeconds after a scheduled time a run may still start.
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`
	// ConcurrencyPolicy is Allow (default), Forbid or Replace.
	ConcurrencyPolicy          string          `json:"concurrencyPolicy,omitempty"`
	Suspend                    *bool           `json:"suspend,omitempty"`
	JobTemplate                JobTemplateSpec `json:"jobTemplate"`
	SuccessfulJobsHistoryLimit *int32          `json:"successfulJobsHistoryLimit,omitempty"`
	FailedJobsHistoryLimit     *int32          `json:"failedJobsHistoryLimit,omitempty"`
}

// Concurrency policies.
const (
	AllowConcurrent   = "Allow"
	ForbidConcurrent  = "Forbid"
	ReplaceConcurrent = "Replace"
)

// JobTemplateSpec is the Job a CronJob creates.
type JobTemplateSpec struct {
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              JobSpec `json:"spec"`
}

// CronJobStatus is maintained by the CronJob controller.
type CronJobStatus struct {
	Active             []corev1.ObjectReference `json:"active,omitempty"`
	LastScheduleTime   *metav1.Time             `json:"lastScheduleTime,omitempty"`
	LastSuccessfulTime *metav1.Time             `json:"lastSuccessfulTime,omitempty"`
}

// SetDefaultsJobSpec fills omitted fields with Kubernetes' defaults.
func SetDefaultsJobSpec(s *JobSpec) {
	if s.Completions == nil && s.Parallelism == nil {
		s.Completions = ptr.To[int32](1)
	}
	if s.Parallelism == nil {
		s.Parallelism = ptr.To[int32](1)
	}
	if s.BackoffLimit == nil {
		s.BackoffLimit = ptr.To[int32](6)
	}
	if s.CompletionMode == nil {
		s.CompletionMode = ptr.To(NonIndexedCompletion)
	}
	if s.Suspend == nil {
		s.Suspend = ptr.To(false)
	}
	if s.ManualSelector == nil {
		s.ManualSelector = ptr.To(false)
	}
	if s.PodReplacementPolicy == nil {
		s.PodReplacementPolicy = ptr.To("TerminatingOrFailed")
	}
	corev1.SetDefaultsPodSpec(&s.Template.Spec)
}

// SetDefaultsCronJob fills omitted fields with Kubernetes' defaults.
func SetDefaultsCronJob(cj *CronJob) {
	if cj.Spec.ConcurrencyPolicy == "" {
		cj.Spec.ConcurrencyPolicy = AllowConcurrent
	}
	if cj.Spec.Suspend == nil {
		cj.Spec.Suspend = ptr.To(false)
	}
	if cj.Spec.SuccessfulJobsHistoryLimit == nil {
		cj.Spec.SuccessfulJobsHistoryLimit = ptr.To[int32](3)
	}
	if cj.Spec.FailedJobsHistoryLimit == nil {
		cj.Spec.FailedJobsHistoryLimit = ptr.To[int32](1)
	}
	corev1.SetDefaultsPodSpec(&cj.Spec.JobTemplate.Spec.Template.Spec)
}
