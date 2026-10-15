package cronjob

import (
	"fmt"
	"sort"
	"time"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/cron"
	"mini-k8s/pkg/util/deepcopy"
)

// nextScheduleDelta pads requeues so the scheduled time has surely passed.
const nextScheduleDelta = 100 * time.Millisecond

// tooManyMissed is the number of missed schedules worth a warning.
const tooManyMissed = 100

// mostRecentScheduleTime returns the time the controller looks back to
// (the last schedule, or the creation), the latest scheduled time not
// after now that has not run yet (nil if none), and how many were missed.
// With withDeadline the look-back stops at startingDeadlineSeconds.
func mostRecentScheduleTime(cj *batchv1.CronJob, now time.Time, sched *cron.Schedule, withDeadline bool) (time.Time, *time.Time, int64, error) {
	earliest := cj.CreationTimestamp.Time
	if cj.Status.LastScheduleTime != nil {
		earliest = cj.Status.LastScheduleTime.Time
	}
	if d := cj.Spec.StartingDeadlineSeconds; withDeadline && d != nil {
		if limit := now.Add(-time.Duration(*d) * time.Second); limit.After(earliest) {
			earliest = limit
		}
	}
	earliest = earliest.In(now.Location()) // the schedule is read in now's time zone
	t1 := sched.Next(earliest)
	if t1.IsZero() || now.Before(t1) {
		return earliest, nil, 0, nil
	}
	t2 := sched.Next(t1)
	if t2.IsZero() || now.Before(t2) {
		return earliest, &t1, 1, nil
	}
	// Skip ahead by the period between the first two times, then walk the
	// last stretch exactly (the period of irregular schedules varies).
	period := int64(t2.Sub(t1).Round(time.Second).Seconds())
	if period < 1 {
		return earliest, nil, 0, fmt.Errorf("time difference between two schedules is less than 1 second")
	}
	missed := int64(now.Sub(t1).Seconds())/period + 1
	var recent time.Time
	for t := sched.Next(t1.Add(time.Duration((missed-2)*period) * time.Second)); !t.IsZero() && !t.After(now); t = sched.Next(t) {
		recent = t
	}
	if recent.IsZero() {
		// The skip overshot (an irregular schedule): walk from the start.
		for t := t1; !t.IsZero() && !t.After(now); t = sched.Next(t) {
			recent = t
		}
	}
	return earliest, &recent, missed, nil
}

// nextScheduleDuration is how long until the schedule after the most
// recent one (or after the look-back time), padded.
func nextScheduleDuration(cj *batchv1.CronJob, now time.Time, sched *cron.Schedule) *time.Duration {
	earliest, recent, _, err := mostRecentScheduleTime(cj, now, sched, false)
	if err != nil {
		return nil
	}
	if recent == nil {
		recent = &earliest
	}
	next := sched.Next(*recent)
	if next.IsZero() {
		return nil
	}
	d := next.Add(nextScheduleDelta).Sub(now)
	return &d
}

// jobName is deterministic per scheduled time, so a time runs at most once.
func jobName(cj *batchv1.CronJob, scheduled time.Time) string {
	return fmt.Sprintf("%s-%d", cj.Name, scheduled.Unix()/60)
}

// location is the CronJob's time zone; without one the controller's.
func location(cj *batchv1.CronJob) (*time.Location, error) {
	if cj.Spec.TimeZone == nil {
		return time.Local, nil
	}
	return time.LoadLocation(*cj.Spec.TimeZone)
}

// jobFromTemplate builds the Job the CronJob runs at the scheduled time.
func jobFromTemplate(cj *batchv1.CronJob, scheduled time.Time) *batchv1.Job {
	tpl := deepcopy.Copy(&cj.Spec.JobTemplate)
	annotations := map[string]string{}
	for k, v := range tpl.Annotations {
		annotations[k] = v
	}
	zone := time.UTC
	if cj.Spec.TimeZone != nil {
		if loc, err := time.LoadLocation(*cj.Spec.TimeZone); err == nil {
			zone = loc
		}
	}
	annotations[batchv1.CronJobScheduledAnno] = scheduled.In(zone).Format(time.RFC3339)
	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: jobName(cj, scheduled), Namespace: cj.Namespace, Labels: tpl.Labels,
			Annotations: annotations, OwnerReferences: []metav1.OwnerReference{metav1.NewControllerRef(cj)}},
		Spec: tpl.Spec,
	}
}

func jobRef(j *batchv1.Job) corev1.ObjectReference {
	return corev1.ObjectReference{Kind: "Job", APIVersion: "batch/v1", Namespace: j.Namespace, Name: j.Name,
		UID: j.UID, ResourceVersion: j.ResourceVersion}
}

func inActiveList(cj *batchv1.CronJob, uid string) bool {
	for _, r := range cj.Status.Active {
		if r.UID == uid {
			return true
		}
	}
	return false
}

func inActiveListByName(cj *batchv1.CronJob, name string) bool {
	for _, r := range cj.Status.Active {
		if r.Name == name {
			return true
		}
	}
	return false
}

func deleteFromActiveList(cj *batchv1.CronJob, uid string) {
	var kept []corev1.ObjectReference
	for _, r := range cj.Status.Active {
		if r.UID != uid {
			kept = append(kept, r)
		}
	}
	cj.Status.Active = kept
}

// sortByStartTime orders Jobs oldest first; Jobs that never started last.
func sortByStartTime(jobs []*batchv1.Job) {
	sort.SliceStable(jobs, func(i, j int) bool {
		a, b := jobs[i].Status.StartTime, jobs[j].Status.StartTime
		switch {
		case a == nil && b != nil:
			return false
		case a != nil && b == nil:
			return true
		case a == nil || a.Equal(b.Time):
			return jobs[i].Name < jobs[j].Name
		}
		return a.Before(b.Time)
	})
}
