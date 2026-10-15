// Package job implements the Job controller, like
// k8s.io/kubernetes/pkg/controller/job: it runs a Job's Pods until enough
// succeed (per index for Indexed Jobs), within parallelism, replaces failed
// ones after a growing backoff and fails the Job when its backoffLimit or
// activeDeadlineSeconds is exceeded. Pods are counted as they are (the
// legacy tracking, without the job-tracking finalizer).
package job

import (
	"context"
	"reflect"
	"sync"
	"time"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/client/workqueue"
	"mini-k8s/pkg/controller"
	"mini-k8s/pkg/util/deepcopy"
	"mini-k8s/pkg/util/ptr"
)

// Condition reasons and messages, as the job-controller words them.
const (
	reasonCompletions = "CompletionsReached"
	msgCompletions    = "Reached expected number of succeeded pods"
	reasonBackoff     = "BackoffLimitExceeded"
	msgBackoff        = "Job has reached the specified backoff limit"
	reasonDeadline    = "DeadlineExceeded"
	msgDeadline       = "Job was active longer than specified deadline"
)

// Controller runs Jobs.
type Controller struct {
	client   *client.Client
	jobs     *informers.Informer[batchv1.Job, *batchv1.Job]
	pods     *informers.Informer[corev1.Pod, *corev1.Pod]
	recorder *record.Recorder
	queue    *workqueue.Queue
	expect   *controller.Expectations
	now      func() time.Time
}

// NewController wires the controller onto shared informers.
func NewController(c *client.Client, jobs *informers.Informer[batchv1.Job, *batchv1.Job],
	pods *informers.Informer[corev1.Pod, *corev1.Pod], recorder *record.Recorder) *Controller {
	jc := &Controller{client: c, jobs: jobs, pods: pods, recorder: recorder, queue: workqueue.New(),
		expect: controller.NewExpectations(), now: time.Now}
	enqueue := func(j *batchv1.Job) { jc.queue.Add(informers.Key(j)) }
	jobs.AddHandler(informers.Handler[*batchv1.Job]{
		OnAdd: enqueue, OnUpdate: func(_, j *batchv1.Job) { enqueue(j) }, OnDelete: enqueue,
	})
	pods.AddHandler(informers.Handler[*corev1.Pod]{
		OnAdd: func(p *corev1.Pod) {
			if key := controller.OwnerKey(p, "Job"); key != "" {
				jc.expect.ObserveAdd(key)
				jc.queue.Add(key)
			}
		},
		OnUpdate: func(old, cur *corev1.Pod) {
			if key := controller.OwnerKey(cur, "Job"); key != "" {
				if cur.DeletionTimestamp != nil && old.DeletionTimestamp == nil {
					jc.expect.ObserveDelete(key, cur.UID)
				}
				jc.queue.Add(key)
			}
		},
		OnDelete: func(p *corev1.Pod) {
			if key := controller.OwnerKey(p, "Job"); key != "" {
				jc.expect.ObserveDelete(key, p.UID)
				jc.queue.Add(key)
			}
		},
	})
	return jc
}

// Run processes Jobs until ctx ends.
func (jc *Controller) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, jc.queue, "job", workers, jc.sync)
}

// state is what a sync sees of a Job's Pods.
type state struct {
	active, succeededPods, failedPods, terminating []*corev1.Pod
	succeededIndexes                               map[int]bool
}

func (jc *Controller) observe(j *batchv1.Job) state {
	var st state
	st.succeededIndexes = map[int]bool{}
	for _, p := range jc.pods.List(j.Namespace, nil) {
		if ref := metav1.ControllerOf(&p.ObjectMeta); ref == nil || ref.UID != j.UID {
			continue
		}
		switch {
		case p.Status.Phase == corev1.PodSucceeded:
			st.succeededPods = append(st.succeededPods, p)
			if i := podIndex(p); i >= 0 {
				st.succeededIndexes[i] = true
			}
		case p.Status.Phase == corev1.PodFailed:
			st.failedPods = append(st.failedPods, p)
		case p.DeletionTimestamp != nil:
			st.terminating = append(st.terminating, p)
		default:
			st.active = append(st.active, p)
		}
	}
	return st
}

func (jc *Controller) sync(ctx context.Context, key string) error {
	ns, name := informers.SplitKey(key)
	cached, ok := jc.jobs.Get(ns, name)
	if !ok {
		jc.expect.Forget(key)
		return nil
	}
	if cached.DeletionTimestamp != nil || !jc.expect.Satisfied(key) {
		return nil
	}
	j := deepcopy.Copy(cached)
	batchv1.SetDefaultsJobSpec(&j.Spec)
	st := jc.observe(j)
	if finished, _ := batchv1.IsFinished(j); finished {
		// A finished Job runs nothing (its active Pods were deleted when it
		// finished; this catches ones that slipped through).
		return jc.deletePods(ctx, j, st.active)
	}
	now := jc.now()
	status := deepcopy.Copy(&j.Status)
	indexed := *j.Spec.CompletionMode == batchv1.IndexedCompletion
	succeeded := int32(len(st.succeededPods))
	var doneIndexes []int
	if indexed {
		for i := range st.succeededIndexes {
			if i < int(*j.Spec.Completions) {
				doneIndexes = append(doneIndexes, i)
			}
		}
		succeeded = int32(len(doneIndexes))
	}
	failed := int32(len(st.failedPods))
	suspended := *j.Spec.Suspend
	if status.StartTime == nil && !suspended {
		status.StartTime = &metav1.Time{Time: now}
	}

	// Failures win over completion, as in Kubernetes.
	var finish *batchv1.JobCondition
	switch {
	case failed > *j.Spec.BackoffLimit || pastBackoffLimitOnFailure(j, st.active):
		finish = &batchv1.JobCondition{Type: batchv1.JobFailed, Reason: reasonBackoff, Message: msgBackoff}
	case j.Spec.ActiveDeadlineSeconds != nil && status.StartTime != nil && !suspended &&
		now.Sub(status.StartTime.Time) >= time.Duration(*j.Spec.ActiveDeadlineSeconds)*time.Second:
		finish = &batchv1.JobCondition{Type: batchv1.JobFailed, Reason: reasonDeadline, Message: msgDeadline}
	case j.Spec.Completions == nil && len(st.succeededPods) > 0 && len(st.active) == 0,
		j.Spec.Completions != nil && succeeded >= *j.Spec.Completions:
		finish = &batchv1.JobCondition{Type: batchv1.JobComplete, Reason: reasonCompletions, Message: msgCompletions}
	}

	var err error
	switch {
	case finish != nil:
		err = jc.deletePods(ctx, j, st.active)
		st.terminating = append(st.terminating, st.active...)
		st.active = nil
		mark := metav1.Time{Time: now}
		if finish.Type == batchv1.JobComplete {
			setCondition(status, batchv1.JobCondition{Type: batchv1.JobSuccessCriteriaMet, Reason: finish.Reason, Message: finish.Message}, mark)
			status.CompletionTime = &mark
			jc.recorder.Eventf(j, corev1.EventTypeNormal, "Completed", "Job completed")
		} else {
			setCondition(status, batchv1.JobCondition{Type: batchv1.JobFailureTarget, Reason: finish.Reason, Message: finish.Message}, mark)
			jc.recorder.Eventf(j, corev1.EventTypeWarning, finish.Reason, "%s", finish.Message)
		}
		setCondition(status, *finish, mark)
	case suspended:
		err = jc.deletePods(ctx, j, st.active)
		st.terminating = append(st.terminating, st.active...)
		st.active = nil
		if setCondition(status, batchv1.JobCondition{Type: batchv1.JobSuspended, Reason: "JobSuspended", Message: "Job suspended"},
			metav1.Time{Time: now}) {
			jc.recorder.Eventf(j, corev1.EventTypeNormal, "Suspended", "Job suspended")
		}
	default:
		if c := status.Condition(batchv1.JobSuspended); c != nil && c.Status == corev1.ConditionTrue {
			setConditionStatus(status, batchv1.JobSuspended, corev1.ConditionFalse, "JobResumed", "Job resumed", metav1.Time{Time: now})
			jc.recorder.Eventf(j, corev1.EventTypeNormal, "Resumed", "Job resumed")
			// The deadline counts the time the Job is continuously active.
			status.StartTime = &metav1.Time{Time: now}
		}
		err = jc.manage(ctx, j, &st, succeeded, now)
		if d := j.Spec.ActiveDeadlineSeconds; d != nil {
			jc.queue.AddAfter(key, status.StartTime.Add(time.Duration(*d)*time.Second).Sub(now)+100*time.Millisecond)
		}
	}

	status.Active, status.Succeeded, status.Failed = int32(len(st.active)), succeeded, failed
	ready := int32(0)
	for _, p := range st.active {
		if corev1.IsPodReady(p) {
			ready++
		}
	}
	status.Ready, status.Terminating = ptr.To(ready), ptr.To(int32(len(st.terminating)))
	status.UncountedTerminatedPods = &batchv1.UncountedTerminatedPods{}
	if indexed {
		status.CompletedIndexes = FormatIndexes(doneIndexes)
	}
	if !reflect.DeepEqual(*status, cached.Status) {
		cp := deepcopy.Copy(cached)
		cp.Status = *status
		_, uerr := client.Jobs(jc.client, ns).UpdateStatus(ctx, cp)
		if uerr != nil && !client.IsConflict(uerr) && !client.IsNotFound(uerr) && err == nil {
			err = uerr
		}
	}
	return err
}

// manage creates or deletes Pods so that the right number run: up to
// parallelism, and no more than the successes still missing (per pending
// index for Indexed Jobs). After failures, new Pods wait for a backoff.
func (jc *Controller) manage(ctx context.Context, j *batchv1.Job, st *state, succeeded int32, now time.Time) error {
	parallelism := int(*j.Spec.Parallelism)
	var want int
	var pendingIndexes []int
	switch {
	case *j.Spec.CompletionMode == batchv1.IndexedCompletion:
		running := map[int]bool{}
		for _, p := range st.active {
			running[podIndex(p)] = true
		}
		for i := 0; i < int(*j.Spec.Completions); i++ {
			if !st.succeededIndexes[i] && !running[i] {
				pendingIndexes = append(pendingIndexes, i)
			}
		}
		want = min(parallelism, len(st.active)+len(pendingIndexes))
	case j.Spec.Completions == nil:
		want = parallelism
		if len(st.succeededPods) > 0 {
			// A work queue: once one Pod succeeded, the others finish their
			// work but no new ones start.
			want = len(st.active)
		}
	default:
		want = min(parallelism, int(*j.Spec.Completions-succeeded))
	}
	switch diff := want - len(st.active); {
	case diff < 0:
		active := append([]*corev1.Pod{}, st.active...)
		rankForDeletion(active)
		extra := active[:-diff]
		st.active = active[-diff:]
		st.terminating = append(st.terminating, extra...)
		return jc.deletePods(ctx, j, extra)
	case diff > 0:
		if n, last := recentFailures(st.succeededPods, st.failedPods); n > 0 {
			if wait := last.Add(failureBackoff(n)).Sub(now); wait > 0 {
				jc.queue.AddAfter(informers.Key(j), wait)
				return nil
			}
		}
		indexes := make([]int, diff)
		for i := range indexes {
			indexes[i] = -1
			if i < len(pendingIndexes) {
				indexes[i] = pendingIndexes[i]
			}
		}
		return jc.createPods(ctx, j, indexes)
	}
	return nil
}

func (jc *Controller) createPods(ctx context.Context, j *batchv1.Job, indexes []int) error {
	key := informers.Key(j)
	jc.expect.Expect(key, len(indexes), nil)
	var wg sync.WaitGroup
	errs := make(chan error, len(indexes))
	for _, index := range indexes {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			p, err := client.Pods(jc.client, j.Namespace).Create(ctx, NewPod(j, index))
			if err != nil {
				jc.expect.ObserveAdd(key)
				jc.recorder.Eventf(j, corev1.EventTypeWarning, "FailedCreate", "Error creating: %v", err)
				errs <- err
				return
			}
			jc.recorder.Eventf(j, corev1.EventTypeNormal, "SuccessfulCreate", "Created pod: %s", p.Name)
		}(index)
	}
	wg.Wait()
	close(errs)
	return <-errs
}

func (jc *Controller) deletePods(ctx context.Context, j *batchv1.Job, pods []*corev1.Pod) error {
	if len(pods) == 0 {
		return nil
	}
	key := informers.Key(j)
	uids := make([]string, len(pods))
	for i, p := range pods {
		uids[i] = p.UID
	}
	jc.expect.Expect(key, 0, uids)
	var firstErr error
	for _, p := range pods {
		err := client.Pods(jc.client, p.Namespace).Delete(ctx, p.Name, &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &p.UID}})
		if err != nil && !client.IsNotFound(err) {
			jc.expect.ObserveDelete(key, p.UID)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		jc.recorder.Eventf(j, corev1.EventTypeNormal, "SuccessfulDelete", "Deleted pod: %s", p.Name)
	}
	return firstErr
}

// setCondition sets a True condition, keeping the transition time if it
// was already True; it reports whether anything changed.
func setCondition(s *batchv1.JobStatus, c batchv1.JobCondition, now metav1.Time) bool {
	return setConditionStatus(s, c.Type, corev1.ConditionTrue, c.Reason, c.Message, now)
}

func setConditionStatus(s *batchv1.JobStatus, typ, status, reason, message string, now metav1.Time) bool {
	if cur := s.Condition(typ); cur != nil {
		if cur.Status == status && cur.Reason == reason && cur.Message == message {
			return false
		}
		if cur.Status != status {
			cur.LastTransitionTime = now
		}
		cur.Status, cur.Reason, cur.Message, cur.LastProbeTime = status, reason, message, now
		return true
	}
	s.Conditions = append(s.Conditions, batchv1.JobCondition{Type: typ, Status: status, Reason: reason, Message: message,
		LastProbeTime: now, LastTransitionTime: now})
	return true
}
