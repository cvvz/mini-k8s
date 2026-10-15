// Package ttlafterfinished implements the TTL-after-finished controller,
// like k8s.io/kubernetes/pkg/controller/ttlafterfinished: it deletes a
// finished Job (and, through the garbage collector, its Pods) once
// spec.ttlSecondsAfterFinished has passed since it finished.
package ttlafterfinished

import (
	"context"
	"time"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/workqueue"
	"mini-k8s/pkg/util/ptr"
)

// Controller deletes expired finished Jobs.
type Controller struct {
	client *client.Client
	jobs   *informers.Informer[batchv1.Job, *batchv1.Job]
	queue  *workqueue.Queue
	now    func() time.Time
}

// NewController wires the controller onto the shared Job informer.
func NewController(c *client.Client, jobs *informers.Informer[batchv1.Job, *batchv1.Job]) *Controller {
	tc := &Controller{client: c, jobs: jobs, queue: workqueue.New(), now: time.Now}
	enqueue := func(j *batchv1.Job) {
		if j.DeletionTimestamp == nil && j.Spec.TTLSecondsAfterFinished != nil {
			if finished, _ := batchv1.IsFinished(j); finished {
				tc.queue.Add(informers.Key(j))
			}
		}
	}
	jobs.AddHandler(informers.Handler[*batchv1.Job]{OnAdd: enqueue, OnUpdate: func(_, j *batchv1.Job) { enqueue(j) }})
	return tc
}

// Run processes Jobs until ctx ends.
func (tc *Controller) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, tc.queue, "ttl-after-finished", workers, tc.sync)
}

// expiresAt is when a finished Job with a TTL may be deleted, or false.
func expiresAt(j *batchv1.Job) (time.Time, bool) {
	if j.DeletionTimestamp != nil || j.Spec.TTLSecondsAfterFinished == nil {
		return time.Time{}, false
	}
	finished, typ := batchv1.IsFinished(j)
	if !finished {
		return time.Time{}, false
	}
	at := j.Status.Condition(typ).LastTransitionTime.Time
	if typ == batchv1.JobComplete && j.Status.CompletionTime != nil {
		at = j.Status.CompletionTime.Time
	}
	return at.Add(time.Duration(*j.Spec.TTLSecondsAfterFinished) * time.Second), true
}

func (tc *Controller) sync(ctx context.Context, key string) error {
	ns, name := informers.SplitKey(key)
	cached, ok := tc.jobs.Get(ns, name)
	if !ok {
		return nil
	}
	at, ok := expiresAt(cached)
	if !ok {
		return nil
	}
	if wait := at.Sub(tc.now()); wait > 0 {
		tc.queue.AddAfter(key, wait)
		return nil
	}
	// The cache may be stale: check the TTL (which may have been changed)
	// on the latest Job, and delete exactly that version.
	fresh, err := client.Jobs(tc.client, ns).Get(ctx, name)
	if err != nil {
		if client.IsNotFound(err) {
			return nil
		}
		return err
	}
	if at, ok = expiresAt(fresh); !ok {
		return nil
	}
	if wait := at.Sub(tc.now()); wait > 0 {
		tc.queue.AddAfter(key, wait)
		return nil
	}
	err = client.Jobs(tc.client, ns).Delete(ctx, name, &metav1.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationForeground),
		Preconditions:     &metav1.Preconditions{UID: &fresh.UID, ResourceVersion: &fresh.ResourceVersion},
	})
	if client.IsNotFound(err) || client.IsConflict(err) {
		return nil // gone, or changed: the update requeues it
	}
	return err
}
