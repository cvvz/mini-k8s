// Package deployment implements the Deployment controller.
package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/client/workqueue"
	"mini-k8s/pkg/controller"
	"mini-k8s/pkg/util/deepcopy"
)

// DeploymentController keeps the number of Pods built from each Deployment's
// current template equal to spec.replicas. Template changes replace Pods
// with the Recreate strategy: old Pods are removed before new ones start.
type DeploymentController struct {
	client      *client.Client
	deployments *informers.Informer[appsv1.Deployment, *appsv1.Deployment]
	pods        *informers.Informer[corev1.Pod, *corev1.Pod]
	recorder    *record.Recorder
	queue       *workqueue.Queue
	expect      *controller.Expectations
}

// NewDeploymentController wires event handlers onto the shared informers.
func NewDeploymentController(c *client.Client, deployments *informers.Informer[appsv1.Deployment, *appsv1.Deployment],
	pods *informers.Informer[corev1.Pod, *corev1.Pod], recorder *record.Recorder) *DeploymentController {
	dc := &DeploymentController{client: c, deployments: deployments, pods: pods, recorder: recorder,
		queue: workqueue.New(), expect: controller.NewExpectations()}
	enqueue := func(d *appsv1.Deployment) { dc.queue.Add(informers.Key(d)) }
	deployments.AddHandler(informers.Handler[*appsv1.Deployment]{
		OnAdd: enqueue, OnUpdate: func(_, d *appsv1.Deployment) { enqueue(d) }, OnDelete: enqueue,
	})
	pods.AddHandler(informers.Handler[*corev1.Pod]{
		OnAdd: func(p *corev1.Pod) {
			if key := controller.OwnerKey(p, "Deployment"); key != "" {
				dc.expect.ObserveAdd(key)
				dc.queue.Add(key)
			}
		},
		OnUpdate: func(old, cur *corev1.Pod) {
			if key := controller.OwnerKey(cur, "Deployment"); key != "" {
				if cur.DeletionTimestamp != nil && old.DeletionTimestamp == nil {
					dc.expect.ObserveDelete(key, cur.UID)
				}
				dc.queue.Add(key)
			}
		},
		OnDelete: func(p *corev1.Pod) {
			if key := controller.OwnerKey(p, "Deployment"); key != "" {
				dc.expect.ObserveDelete(key, p.UID)
				dc.queue.Add(key)
			}
		},
	})
	return dc
}

// Run processes Deployments until ctx ends.
func (dc *DeploymentController) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, dc.queue, "deployment", workers, dc.sync)
}

// TemplateHash identifies a Pod template; it is stored in the
// pod-template-hash label of every Pod built from it.
func TemplateHash(tpl *corev1.PodTemplateSpec) string {
	data, _ := json.Marshal(tpl)
	h := fnv.New32a()
	_, _ = h.Write(data)
	return safeEncode(fmt.Sprint(h.Sum32()))
}

// safeEncode maps digits onto consonants and digits that cannot spell words.
func safeEncode(s string) string {
	const alphabet = "bcdfghjklmnpqrstvwxz2456789"
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		out[i] = alphabet[int(s[i])%len(alphabet)]
	}
	return string(out)
}

func (dc *DeploymentController) sync(ctx context.Context, key string) error {
	ns, name := informers.SplitKey(key)
	d, ok := dc.deployments.Get(ns, name)
	if !ok {
		dc.expect.Forget(key)
		return nil
	}
	if d.DeletionTimestamp != nil || !dc.expect.Satisfied(key) {
		return nil
	}
	sel, err := d.Spec.Selector.AsSelector()
	if err != nil {
		return err
	}
	hash := TemplateHash(&d.Spec.Template)
	var active, current, outdated []*corev1.Pod
	oldTerminating := false
	for _, p := range dc.pods.List(ns, sel) {
		if ref := metav1.ControllerOf(&p.ObjectMeta); ref == nil || ref.UID != d.UID {
			continue
		}
		isCurrent := p.Labels[appsv1.DefaultDeploymentUniqueLabelKey] == hash
		if p.DeletionTimestamp != nil {
			oldTerminating = oldTerminating || !isCurrent
			continue
		}
		active = append(active, p)
		if isCurrent {
			current = append(current, p)
		} else {
			outdated = append(outdated, p)
		}
	}
	want := int(*d.Spec.Replicas)
	deletes := outdated
	creates := 0
	if diff := want - len(current); diff < 0 {
		rankForDeletion(current)
		deletes = append(deletes, current[:-diff]...)
	} else if len(outdated) == 0 && !oldTerminating {
		// Recreate: new Pods start only after every old Pod is gone.
		creates = diff
	}
	uids := make([]string, len(deletes))
	for i, p := range deletes {
		uids[i] = p.UID
	}
	dc.expect.Expect(key, creates, uids)
	if err := dc.manage(ctx, d, hash, creates, deletes); err != nil {
		return err
	}
	return dc.updateStatus(ctx, d, active, current)
}

func (dc *DeploymentController) manage(ctx context.Context, d *appsv1.Deployment, hash string, creates int, deletes []*corev1.Pod) error {
	key := informers.Key(d)
	var wg sync.WaitGroup
	errs := make(chan error, creates+len(deletes))
	for i := 0; i < creates; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := client.Pods(dc.client, d.Namespace).Create(ctx, podFromTemplate(d, hash))
			if err != nil {
				dc.expect.ObserveAdd(key)
				dc.recorder.Eventf(d, corev1.EventTypeWarning, "FailedCreate", "Error creating pod: %v", err)
				errs <- err
				return
			}
			dc.recorder.Eventf(d, corev1.EventTypeNormal, "SuccessfulCreate", "Created pod: %s", p.Name)
		}()
	}
	for _, p := range deletes {
		wg.Add(1)
		go func(p *corev1.Pod) {
			defer wg.Done()
			err := client.Pods(dc.client, p.Namespace).Delete(ctx, p.Name, &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &p.UID}})
			if err != nil && !client.IsNotFound(err) {
				dc.expect.ObserveDelete(key, p.UID)
				errs <- err
				return
			}
			dc.recorder.Eventf(d, corev1.EventTypeNormal, "SuccessfulDelete", "Deleted pod: %s", p.Name)
		}(p)
	}
	wg.Wait()
	close(errs)
	return <-errs
}

func podFromTemplate(d *appsv1.Deployment, hash string) *corev1.Pod {
	tpl := deepcopy.Copy(&d.Spec.Template)
	labels := map[string]string{appsv1.DefaultDeploymentUniqueLabelKey: hash}
	for k, v := range tpl.Labels {
		labels[k] = v
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: d.Name + "-" + hash + "-", Namespace: d.Namespace,
			Labels: labels, Annotations: tpl.Annotations,
			OwnerReferences: []metav1.OwnerReference{metav1.NewControllerRef(d)},
		},
		Spec: tpl.Spec,
	}
}

// rankForDeletion orders Pods so the cheapest to lose come first:
// unscheduled, then pending, then not ready, then the newest.
func rankForDeletion(pods []*corev1.Pod) {
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

func (dc *DeploymentController) updateStatus(ctx context.Context, d *appsv1.Deployment, active, current []*corev1.Pod) error {
	ready := int32(0)
	for _, p := range active {
		if corev1.IsPodReady(p) {
			ready++
		}
	}
	want := *d.Spec.Replicas
	st := appsv1.DeploymentStatus{
		ObservedGeneration:  d.Generation,
		Replicas:            int32(len(active)),
		UpdatedReplicas:     int32(len(current)),
		ReadyReplicas:       ready,
		AvailableReplicas:   ready,
		UnavailableReplicas: max(want-ready, 0),
	}
	if st == d.Status {
		return nil
	}
	cp := deepcopy.Copy(d)
	cp.Status = st
	_, err := client.Deployments(dc.client, d.Namespace).UpdateStatus(ctx, cp)
	if client.IsConflict(err) || client.IsNotFound(err) {
		return nil // a newer version is already queued
	}
	return err
}
