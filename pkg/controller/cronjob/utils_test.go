package cronjob

import (
	"testing"
	"time"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/cron"
	"mini-k8s/pkg/util/ptr"
)

func mustParse(t *testing.T, spec string) *cron.Schedule {
	t.Helper()
	s, err := cron.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMostRecentScheduleTime(t *testing.T) {
	created := time.Date(2026, 10, 15, 8, 0, 30, 0, time.UTC)
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "c", CreationTimestamp: metav1.Time{Time: created}}}
	every5 := mustParse(t, "*/5 * * * *")

	// Not yet due.
	if _, got, missed, err := mostRecentScheduleTime(cj, created.Add(time.Minute), every5, true); err != nil || got != nil || missed != 0 {
		t.Fatalf("not due: %v %d %v", got, missed, err)
	}
	// One due time.
	if _, got, missed, _ := mostRecentScheduleTime(cj, time.Date(2026, 10, 15, 8, 6, 0, 0, time.UTC), every5, true); got == nil ||
		!got.Equal(time.Date(2026, 10, 15, 8, 5, 0, 0, time.UTC)) || missed != 1 {
		t.Fatalf("one due: %v %d", got, missed)
	}
	// Many missed: only the latest is returned.
	now := time.Date(2026, 10, 15, 10, 2, 0, 0, time.UTC)
	_, got, missed, _ := mostRecentScheduleTime(cj, now, every5, true)
	if got == nil || !got.Equal(time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)) || missed != 24 {
		t.Fatalf("missed: %v %d", got, missed)
	}
	// The last schedule time is where the look-back starts.
	cj.Status.LastScheduleTime = &metav1.Time{Time: time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)}
	if _, got, _, _ := mostRecentScheduleTime(cj, now, every5, true); got != nil {
		t.Fatalf("already ran: %v", got)
	}
	// A starting deadline limits the look-back.
	cj.Status.LastScheduleTime = nil
	cj.Spec.StartingDeadlineSeconds = ptr.To[int64](60)
	if _, got, _, _ := mostRecentScheduleTime(cj, now.Add(5*time.Minute), every5, true); got != nil {
		t.Fatalf("past the deadline: %v", got)
	}
	if _, got, _, _ := mostRecentScheduleTime(cj, now.Add(5*time.Minute), every5, false); got == nil ||
		!got.Equal(time.Date(2026, 10, 15, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("without the deadline: %v", got)
	}
	// Irregular schedules are walked exactly.
	cj.Spec.StartingDeadlineSeconds = nil
	odd := mustParse(t, "30 6-16/4 * * 1-5")
	if _, got, _, _ := mostRecentScheduleTime(cj, time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), odd, true); got == nil ||
		!got.Equal(time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)) {
		t.Fatalf("irregular: %v", got)
	}
	// Time zones: 09:00 in Berlin is 07:00 UTC in October.
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	if _, got, _, _ := mostRecentScheduleTime(cj, time.Date(2026, 10, 16, 7, 30, 0, 0, time.UTC).In(berlin), mustParse(t, "0 9 * * *"), true); got == nil ||
		!got.Equal(time.Date(2026, 10, 16, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("Berlin: %v", got)
	}
}

func TestNextScheduleDuration(t *testing.T) {
	created := time.Date(2026, 10, 15, 8, 0, 30, 0, time.UTC)
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.Time{Time: created}}}
	now := time.Date(2026, 10, 15, 8, 3, 0, 0, time.UTC)
	if d := nextScheduleDuration(cj, now, mustParse(t, "*/5 * * * *")); d == nil || *d != 2*time.Minute+nextScheduleDelta {
		t.Fatalf("before the first run: %v", d)
	}
	now = time.Date(2026, 10, 15, 8, 7, 0, 0, time.UTC)
	if d := nextScheduleDuration(cj, now, mustParse(t, "*/5 * * * *")); d == nil || *d != 3*time.Minute+nextScheduleDelta {
		t.Fatalf("after a due time: %v", d)
	}
	if d := nextScheduleDuration(cj, now, mustParse(t, "0 0 31 2 *")); d != nil {
		t.Fatalf("never: %v", d)
	}
}

func TestJobFromTemplate(t *testing.T) {
	cj := &batchv1.CronJob{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "ns", UID: "cj-uid"},
		Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"team": "a"}, Annotations: map[string]string{"note": "x"}},
			Spec:       batchv1.JobSpec{BackoffLimit: ptr.To[int32](2)}}}}
	at := time.Date(2026, 10, 15, 8, 5, 0, 0, time.UTC)
	j := jobFromTemplate(cj, at)
	if j.Name != "nightly-29867525" || j.Namespace != "ns" || j.Labels["team"] != "a" || j.Annotations["note"] != "x" ||
		j.Annotations[batchv1.CronJobScheduledAnno] != "2026-10-15T08:05:00Z" || *j.Spec.BackoffLimit != 2 {
		t.Fatalf("job: %+v", j)
	}
	if ref := metav1.ControllerOf(&j.ObjectMeta); ref == nil || ref.Kind != "CronJob" || ref.UID != "cj-uid" {
		t.Fatalf("owner: %+v", j.OwnerReferences)
	}
	cj.Spec.TimeZone = ptr.To("Asia/Shanghai")
	if j := jobFromTemplate(cj, at); j.Annotations[batchv1.CronJobScheduledAnno] != "2026-10-15T16:05:00+08:00" {
		t.Fatalf("time zone annotation: %s", j.Annotations[batchv1.CronJobScheduledAnno])
	}
	j.Annotations["changed"] = "y"
	if cj.Spec.JobTemplate.Annotations["changed"] != "" {
		t.Fatal("the template must not change")
	}
}

func TestSortByStartTime(t *testing.T) {
	at := func(m int) *metav1.Time { return &metav1.Time{Time: time.Date(2026, 1, 1, 0, m, 0, 0, time.UTC)} }
	jobs := []*batchv1.Job{
		{ObjectMeta: metav1.ObjectMeta{Name: "none"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "late"}, Status: batchv1.JobStatus{StartTime: at(9)}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b-early"}, Status: batchv1.JobStatus{StartTime: at(1)}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a-early"}, Status: batchv1.JobStatus{StartTime: at(1)}},
	}
	sortByStartTime(jobs)
	var got []string
	for _, j := range jobs {
		got = append(got, j.Name)
	}
	if got[0] != "a-early" || got[1] != "b-early" || got[2] != "late" || got[3] != "none" {
		t.Fatalf("order: %v", got)
	}
}
