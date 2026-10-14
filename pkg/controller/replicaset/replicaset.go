// Package replicaset implements the ReplicaSet controller: it keeps the
// number of active Pods a ReplicaSet controls equal to spec.replicas.
package replicaset

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"time"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/client/workqueue"
	"mini-k8s/pkg/controller"
	"mini-k8s/pkg/labels"
	"mini-k8s/pkg/util/deepcopy"
)

// Controller reconciles ReplicaSets against their Pods.
type Controller struct {
	client   *client.Client
	rsets    *informers.Informer[appsv1.ReplicaSet, *appsv1.ReplicaSet]
	pods     *informers.Informer[corev1.Pod, *corev1.Pod]
	recorder *record.Recorder
	queue    *workqueue.Queue
	expect   *controller.Expectations
	now      func() time.Time
}

// NewController wires event handlers onto the shared informers.
func NewController(c *client.Client, rsets *informers.Informer[appsv1.ReplicaSet, *appsv1.ReplicaSet],
	pods *informers.Informer[corev1.Pod, *corev1.Pod], recorder *record.Recorder) *Controller {
	rc := &Controller{client: c, rsets: rsets, pods: pods, recorder: recorder,
		queue: workqueue.New(), expect: controller.NewExpectations(), now: time.Now}
	enqueue := func(rs *appsv1.ReplicaSet) { rc.queue.Add(informers.Key(rs)) }
	rsets.AddHandler(informers.Handler[*appsv1.ReplicaSet]{
		OnAdd: enqueue, OnUpdate: func(_, rs *appsv1.ReplicaSet) { enqueue(rs) }, OnDelete: enqueue,
	})
	pods.AddHandler(informers.Handler[*corev1.Pod]{
		OnAdd: func(p *corev1.Pod) {
			if key := controller.OwnerKey(p, "ReplicaSet"); key != "" {
				rc.expect.ObserveAdd(key)
				rc.queue.Add(key)
			}
		},
		OnUpdate: func(old, cur *corev1.Pod) {
			if key := controller.OwnerKey(cur, "ReplicaSet"); key != "" {
				if cur.DeletionTimestamp != nil && old.DeletionTimestamp == nil {
					rc.expect.ObserveDelete(key, cur.UID)
				}
				rc.queue.Add(key)
			}
		},
		OnDelete: func(p *corev1.Pod) {
			if key := controller.OwnerKey(p, "ReplicaSet"); key != "" {
				rc.expect.ObserveDelete(key, p.UID)
				rc.queue.Add(key)
			}
		},
	})
	return rc
}

// Run processes ReplicaSets until ctx ends.
func (rc *Controller) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, rc.queue, "replicaset", workers, rc.sync)
}

// IsPodActive reports whether a Pod counts towards a controller's replicas:
// not being deleted and not finished.
func IsPodActive(p *corev1.Pod) bool {
	return p.DeletionTimestamp == nil && !corev1.IsPodTerminal(p)
}

// IsPodAvailable reports whether a Pod has been ready for minReadySeconds,
// and otherwise how long until it may become available.
func IsPodAvailable(p *corev1.Pod, minReadySeconds int32, now time.Time) (bool, time.Duration) {
	c := p.Status.Condition(corev1.PodReady)
	if c == nil || c.Status != corev1.ConditionTrue {
		return false, 0
	}
	wait := time.Duration(minReadySeconds)*time.Second - now.Sub(c.LastTransitionTime.Time)
	if minReadySeconds == 0 || wait <= 0 {
		return true, 0
	}
	return false, wait
}

func (rc *Controller) sync(ctx context.Context, key string) error {
	ns, name := informers.SplitKey(key)
	rs, ok := rc.rsets.Get(ns, name)
	if !ok {
		rc.expect.Forget(key)
		return nil
	}
	if rs.DeletionTimestamp != nil || !rc.expect.Satisfied(key) {
		return nil
	}
	var active []*corev1.Pod
	for _, p := range rc.pods.List(ns, nil) {
		if ref := metav1.ControllerOf(&p.ObjectMeta); ref != nil && ref.UID == rs.UID && IsPodActive(p) {
			active = append(active, p)
		}
	}
	creates, deletes := 0, []*corev1.Pod(nil)
	if diff := int(*rs.Spec.Replicas) - len(active); diff > 0 {
		creates = diff
	} else if diff < 0 {
		RankForDeletion(active)
		deletes = active[:-diff]
	}
	uids := make([]string, len(deletes))
	for i, p := range deletes {
		uids[i] = p.UID
	}
	rc.expect.Expect(key, creates, uids)
	err := rc.manage(ctx, rs, creates, deletes)
	if serr := rc.updateStatus(ctx, rs, active); err == nil {
		err = serr
	}
	return err
}

func (rc *Controller) manage(ctx context.Context, rs *appsv1.ReplicaSet, creates int, deletes []*corev1.Pod) error {
	key := informers.Key(rs)
	var wg sync.WaitGroup
	errs := make(chan error, creates+len(deletes))
	for i := 0; i < creates; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := client.Pods(rc.client, rs.Namespace).Create(ctx, podFromTemplate(rs))
			if err != nil {
				rc.expect.ObserveAdd(key)
				rc.recorder.Eventf(rs, corev1.EventTypeWarning, "FailedCreate", "Error creating: %v", err)
				errs <- err
				return
			}
			rc.recorder.Eventf(rs, corev1.EventTypeNormal, "SuccessfulCreate", "Created pod: %s", p.Name)
		}()
	}
	for _, p := range deletes {
		wg.Add(1)
		go func(p *corev1.Pod) {
			defer wg.Done()
			err := client.Pods(rc.client, p.Namespace).Delete(ctx, p.Name, &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &p.UID}})
			if err != nil && !client.IsNotFound(err) {
				rc.expect.ObserveDelete(key, p.UID)
				errs <- err
				return
			}
			rc.recorder.Eventf(rs, corev1.EventTypeNormal, "SuccessfulDelete", "Deleted pod: %s", p.Name)
		}(p)
	}
	wg.Wait()
	close(errs)
	return <-errs
}

func podFromTemplate(rs *appsv1.ReplicaSet) *corev1.Pod {
	tpl := deepcopy.Copy(&rs.Spec.Template)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: rs.Name + "-", Namespace: rs.Namespace,
			Labels: tpl.Labels, Annotations: tpl.Annotations,
			OwnerReferences: []metav1.OwnerReference{metav1.NewControllerRef(rs)},
		},
		Spec: tpl.Spec,
	}
}

// RankForDeletion orders Pods so the cheapest to lose come first:
// unscheduled, then pending, then not ready, then the newest.
func RankForDeletion(pods []*corev1.Pod) {
	score := func(p *corev1.Pod) int {
		switch {
		case p.Spec.NodeName == "":
			return 0
		case p.Status.Phase == corev1.PodPending:
			return 1
		case !corev1.IsPodReady(p):
			return 2
		}
		return 3
	}
	sort.SliceStable(pods, func(i, j int) bool {
		if si, sj := score(pods[i]), score(pods[j]); si != sj {
			return si < sj
		}
		if !pods[i].CreationTimestamp.Equal(pods[j].CreationTimestamp.Time) {
			return pods[i].CreationTimestamp.After(pods[j].CreationTimestamp.Time)
		}
		return pods[i].Name > pods[j].Name
	})
}

// updateStatus reports the active Pods; Pods that will become available once
// they have been ready for minReadySeconds get the ReplicaSet requeued.
func (rc *Controller) updateStatus(ctx context.Context, rs *appsv1.ReplicaSet, active []*corev1.Pod) error {
	tplLabels := labels.SelectorFromSet(rs.Spec.Template.Labels)
	st := appsv1.ReplicaSetStatus{Replicas: int32(len(active)), ObservedGeneration: rs.Generation}
	var soonest time.Duration
	for _, p := range active {
		if tplLabels.Matches(p.Labels) {
			st.FullyLabeledReplicas++
		}
		if corev1.IsPodReady(p) {
			st.ReadyReplicas++
		}
		available, wait := IsPodAvailable(p, rs.Spec.MinReadySeconds, rc.now())
		if available {
			st.AvailableReplicas++
		} else if wait > 0 && (soonest == 0 || wait < soonest) {
			soonest = wait
		}
	}
	if soonest > 0 {
		rc.queue.AddAfter(informers.Key(rs), soonest)
	}
	if reflect.DeepEqual(st, rs.Status) {
		return nil
	}
	cp := deepcopy.Copy(rs)
	cp.Status = st
	_, err := client.ReplicaSets(rc.client, rs.Namespace).UpdateStatus(ctx, cp)
	if client.IsConflict(err) || client.IsNotFound(err) {
		return nil // a newer version is already queued
	}
	return err
}
