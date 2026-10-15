// Package cronjob implements the CronJob controller, like
// k8s.io/kubernetes/pkg/controller/cronjob (ControllerV2): at each
// scheduled time it creates a Job from the template (only the most recent
// missed time runs, and none past startingDeadlineSeconds), applies the
// concurrency policy, tracks the active Jobs and trims the history.
package cronjob

import (
	"context"
	"reflect"
	"time"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/client/workqueue"
	"mini-k8s/pkg/util/cron"
	"mini-k8s/pkg/util/deepcopy"
	"mini-k8s/pkg/util/ptr"
)

// Controller runs CronJobs.
type Controller struct {
	client   *client.Client
	cronJobs *informers.Informer[batchv1.CronJob, *batchv1.CronJob]
	jobs     *informers.Informer[batchv1.Job, *batchv1.Job]
	recorder *record.Recorder
	queue    *workqueue.Queue
	now      func() time.Time
}

// NewController wires the controller onto shared informers.
func NewController(c *client.Client, cronJobs *informers.Informer[batchv1.CronJob, *batchv1.CronJob],
	jobs *informers.Informer[batchv1.Job, *batchv1.Job], recorder *record.Recorder) *Controller {
	cc := &Controller{client: c, cronJobs: cronJobs, jobs: jobs, recorder: recorder, queue: workqueue.New(), now: time.Now}
	enqueue := func(cj *batchv1.CronJob) { cc.queue.Add(informers.Key(cj)) }
	cronJobs.AddHandler(informers.Handler[*batchv1.CronJob]{
		OnAdd: enqueue, OnUpdate: func(_, cj *batchv1.CronJob) { enqueue(cj) },
	})
	enqueueOwner := func(j *batchv1.Job) {
		if ref := metav1.ControllerOf(&j.ObjectMeta); ref != nil && ref.Kind == "CronJob" {
			cc.queue.Add(j.Namespace + "/" + ref.Name)
		}
	}
	jobs.AddHandler(informers.Handler[*batchv1.Job]{
		OnAdd: enqueueOwner, OnUpdate: func(_, j *batchv1.Job) { enqueueOwner(j) }, OnDelete: enqueueOwner,
	})
	return cc
}

// Run processes CronJobs until ctx ends.
func (cc *Controller) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, cc.queue, "cronjob", workers, cc.sync)
}

func (cc *Controller) sync(ctx context.Context, key string) error {
	ns, name := informers.SplitKey(key)
	if _, ok := cc.cronJobs.Get(ns, name); !ok {
		return nil
	}
	// Read the CronJob afresh: a sync triggered by the Jobs it just created
	// must see the status it just wrote, or it would run the same schedule
	// again; the cache may not have it yet.
	cached, err := client.CronJobs(cc.client, ns).Get(ctx, name)
	if err != nil {
		if client.IsNotFound(err) {
			return nil
		}
		return err
	}
	cj := deepcopy.Copy(cached)
	var jobs []*batchv1.Job
	for _, j := range cc.jobs.List(ns, nil) {
		if ref := metav1.ControllerOf(&j.ObjectMeta); ref != nil && ref.UID == cj.UID {
			jobs = append(jobs, j)
		}
	}
	requeue, err := cc.syncCronJob(ctx, cj, jobs)
	if cj.DeletionTimestamp == nil {
		cc.cleanupFinishedJobs(ctx, cj, jobs)
	}
	if !reflect.DeepEqual(cj.Status, cached.Status) {
		// A conflict retries the whole sync on the newer CronJob; a Job
		// created here is then found (AlreadyExists) and tracked.
		if _, uerr := client.CronJobs(cc.client, ns).UpdateStatus(ctx, cj); uerr != nil && !client.IsNotFound(uerr) && err == nil {
			err = uerr
		}
	}
	if requeue != nil {
		cc.queue.AddAfter(key, *requeue)
	}
	return err
}

// syncCronJob reconciles the active list with the Jobs and starts the Job
// of a due schedule; it returns when to look again.
func (cc *Controller) syncCronJob(ctx context.Context, cj *batchv1.CronJob, jobs []*batchv1.Job) (*time.Duration, error) {
	now := cc.now()
	children := map[string]bool{}
	for _, j := range jobs {
		children[j.UID] = true
		found := inActiveList(cj, j.UID)
		finished, condition := batchv1.IsFinished(j)
		switch {
		case !found && !finished:
			cc.recorder.Eventf(cj, corev1.EventTypeWarning, "UnexpectedJob", "Saw a job that the controller did not create or forgot: %s", j.Name)
		case found && finished:
			deleteFromActiveList(cj, j.UID)
			cc.recorder.Eventf(cj, corev1.EventTypeNormal, "SawCompletedJob", "Saw completed job: %s, condition: %s", j.Name, condition)
		}
		if finished && condition == batchv1.JobComplete && j.Status.CompletionTime != nil &&
			(cj.Status.LastSuccessfulTime == nil || j.Status.CompletionTime.After(cj.Status.LastSuccessfulTime.Time)) {
			cj.Status.LastSuccessfulTime = j.Status.CompletionTime
		}
	}
	// Forget active Jobs that are gone (asking the API server, as the cache
	// may simply lag behind).
	for _, r := range cj.Status.Active {
		if children[r.UID] {
			continue
		}
		if _, err := client.Jobs(cc.client, r.Namespace).Get(ctx, r.Name); client.IsNotFound(err) {
			cc.recorder.Eventf(cj, corev1.EventTypeNormal, "MissingJob", "Active job went missing: %s", r.Name)
			deleteFromActiveList(cj, r.UID)
		} else if err != nil {
			return nil, err
		}
	}

	if cj.DeletionTimestamp != nil || (cj.Spec.Suspend != nil && *cj.Spec.Suspend) {
		return nil, nil
	}
	loc, err := location(cj)
	if err != nil {
		cc.recorder.Eventf(cj, corev1.EventTypeWarning, "UnknownTimeZone", "invalid timeZone: %q: %s", *cj.Spec.TimeZone, err)
		return nil, nil
	}
	sched, err := cron.Parse(cj.Spec.Schedule)
	if err != nil {
		cc.recorder.Eventf(cj, corev1.EventTypeWarning, "InvalidSchedule", "invalid schedule: %s : %s", cj.Spec.Schedule, err)
		return nil, nil
	}
	now = now.In(loc)
	_, scheduled, missed, err := mostRecentScheduleTime(cj, now, sched, true)
	if err != nil {
		cc.recorder.Eventf(cj, corev1.EventTypeWarning, "InvalidSchedule", "invalid schedule: %s : %s", cj.Spec.Schedule, err)
		return nil, nil
	}
	if missed > tooManyMissed {
		cc.recorder.Eventf(cj, corev1.EventTypeWarning, "TooManyMissedTimes",
			"too many missed start times: %d. Set or decrease .spec.startingDeadlineSeconds or check clock skew", missed)
	}
	next := nextScheduleDuration(cj, now, sched)
	if scheduled == nil {
		return next, nil
	}
	if d := cj.Spec.StartingDeadlineSeconds; d != nil && scheduled.Add(time.Duration(*d)*time.Second).Before(now) {
		cc.recorder.Eventf(cj, corev1.EventTypeWarning, "MissSchedule", "Missed scheduled time to start a job: %s",
			scheduled.UTC().Format(time.RFC1123Z))
		return next, nil
	}
	if inActiveListByName(cj, jobName(cj, *scheduled)) ||
		(cj.Status.LastScheduleTime != nil && cj.Status.LastScheduleTime.Equal(*scheduled)) {
		return next, nil // this time already ran
	}
	switch cj.Spec.ConcurrencyPolicy {
	case batchv1.ForbidConcurrent:
		if len(cj.Status.Active) > 0 {
			cc.recorder.Eventf(cj, corev1.EventTypeNormal, "JobAlreadyActive",
				"Not starting job because prior execution is running and concurrency policy is Forbid")
			return next, nil
		}
	case batchv1.ReplaceConcurrent:
		for _, r := range cj.Status.Active {
			j, err := client.Jobs(cc.client, r.Namespace).Get(ctx, r.Name)
			if client.IsNotFound(err) {
				deleteFromActiveList(cj, r.UID)
				continue
			} else if err != nil {
				cc.recorder.Eventf(cj, corev1.EventTypeWarning, "FailedGet", "Get job: %v", err)
				return nil, err
			}
			if err := cc.deleteJob(ctx, cj, j); err != nil {
				return nil, err
			}
		}
	}

	job, err := client.Jobs(cc.client, cj.Namespace).Create(ctx, jobFromTemplate(cj, *scheduled))
	switch {
	case client.IsAlreadyExists(err):
		// Created before, but the status update was lost: take it as ours.
		existing, gerr := client.Jobs(cc.client, cj.Namespace).Get(ctx, jobName(cj, *scheduled))
		if gerr != nil {
			return nil, gerr
		}
		if ref := metav1.ControllerOf(&existing.ObjectMeta); ref == nil || ref.UID != cj.UID {
			return next, nil
		}
		job = existing
	case err != nil:
		cc.recorder.Eventf(cj, corev1.EventTypeWarning, "FailedCreate", "Error creating job: %v", err)
		return nil, err
	default:
		cc.recorder.Eventf(cj, corev1.EventTypeNormal, "SuccessfulCreate", "Created job %s", job.Name)
	}
	if finished, _ := batchv1.IsFinished(job); !finished && !inActiveList(cj, job.UID) {
		cj.Status.Active = append(cj.Status.Active, jobRef(job))
	}
	cj.Status.LastScheduleTime = &metav1.Time{Time: scheduled.UTC()}
	return nextScheduleDuration(cj, now, sched), nil
}

// cleanupFinishedJobs deletes the oldest finished Jobs beyond the history
// limits.
func (cc *Controller) cleanupFinishedJobs(ctx context.Context, cj *batchv1.CronJob, jobs []*batchv1.Job) {
	var succeeded, failed []*batchv1.Job
	for _, j := range jobs {
		if j.DeletionTimestamp != nil {
			continue
		}
		switch _, c := batchv1.IsFinished(j); c {
		case batchv1.JobComplete:
			succeeded = append(succeeded, j)
		case batchv1.JobFailed:
			failed = append(failed, j)
		}
	}
	for _, h := range []struct {
		jobs  []*batchv1.Job
		limit *int32
	}{{succeeded, cj.Spec.SuccessfulJobsHistoryLimit}, {failed, cj.Spec.FailedJobsHistoryLimit}} {
		if h.limit == nil || len(h.jobs) <= int(*h.limit) {
			continue
		}
		sortByStartTime(h.jobs)
		for _, j := range h.jobs[:len(h.jobs)-int(*h.limit)] {
			_ = cc.deleteJob(ctx, cj, j)
		}
	}
}

// deleteJob deletes a Job (its Pods follow in the background) and drops it
// from the active list.
func (cc *Controller) deleteJob(ctx context.Context, cj *batchv1.CronJob, j *batchv1.Job) error {
	err := client.Jobs(cc.client, j.Namespace).Delete(ctx, j.Name, &metav1.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationBackground), Preconditions: &metav1.Preconditions{UID: &j.UID}})
	if err != nil && !client.IsNotFound(err) {
		cc.recorder.Eventf(cj, corev1.EventTypeWarning, "FailedDelete", "Deleted job: %v", err)
		return err
	}
	deleteFromActiveList(cj, j.UID)
	cc.recorder.Eventf(cj, corev1.EventTypeNormal, "SuccessfulDelete", "Deleted job %s", j.Name)
	return nil
}

// SetClock replaces the controller's clock, for tests that need schedules
// to come due without waiting for them.
func (cc *Controller) SetClock(now func() time.Time) { cc.now = now }
