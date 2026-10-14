package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"

	"mini-k8s/internal/api"
	"mini-k8s/internal/client"
	"mini-k8s/internal/informer"
	"mini-k8s/internal/record"
	"mini-k8s/internal/workqueue"
)

// DeploymentController keeps the number of Pods built from each Deployment's
// current template equal to spec.replicas. Template changes replace Pods
// with the Recreate strategy: old Pods are removed before new ones start.
type DeploymentController struct {
	client      *client.Client
	deployments *informer.Informer[api.Deployment, *api.Deployment]
	pods        *informer.Informer[api.Pod, *api.Pod]
	recorder    *record.Recorder
	queue       *workqueue.Queue
	expect      *expectations
}

// NewDeploymentController wires event handlers onto the shared informers.
func NewDeploymentController(c *client.Client, deployments *informer.Informer[api.Deployment, *api.Deployment],
	pods *informer.Informer[api.Pod, *api.Pod], recorder *record.Recorder) *DeploymentController {
	dc := &DeploymentController{client: c, deployments: deployments, pods: pods, recorder: recorder,
		queue: workqueue.New(), expect: newExpectations()}
	enqueue := func(d *api.Deployment) { dc.queue.Add(informer.Key(d)) }
	deployments.AddHandler(informer.Handler[*api.Deployment]{
		OnAdd: enqueue, OnUpdate: func(_, d *api.Deployment) { enqueue(d) }, OnDelete: enqueue,
	})
	pods.AddHandler(informer.Handler[*api.Pod]{
		OnAdd: func(p *api.Pod) {
			if key := ownerKey(p, "Deployment"); key != "" {
				dc.expect.observeAdd(key)
				dc.queue.Add(key)
			}
		},
		OnUpdate: func(old, cur *api.Pod) {
			if key := ownerKey(cur, "Deployment"); key != "" {
				if cur.DeletionTimestamp != nil && old.DeletionTimestamp == nil {
					dc.expect.observeDelete(key, cur.UID)
				}
				dc.queue.Add(key)
			}
		},
		OnDelete: func(p *api.Pod) {
			if key := ownerKey(p, "Deployment"); key != "" {
				dc.expect.observeDelete(key, p.UID)
				dc.queue.Add(key)
			}
		},
	})
	return dc
}

// ownerKey returns "namespace/name" of the managing controller of kind.
func ownerKey(obj api.Object, kind string) string {
	m := obj.GetObjectMeta()
	if ref := api.ControllerOf(m); ref != nil && ref.Kind == kind {
		return m.Namespace + "/" + ref.Name
	}
	return ""
}

// Run processes Deployments until ctx ends.
func (dc *DeploymentController) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, dc.queue, "deployment", workers, dc.sync)
}

// TemplateHash identifies a Pod template; it is stored in the
// pod-template-hash label of every Pod built from it.
func TemplateHash(tpl *api.PodTemplateSpec) string {
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
	ns, name := informer.SplitKey(key)
	d, ok := dc.deployments.Get(ns, name)
	if !ok {
		dc.expect.forget(key)
		return nil
	}
	if d.DeletionTimestamp != nil || !dc.expect.satisfied(key) {
		return nil
	}
	sel, err := d.Spec.Selector.AsSelector()
	if err != nil {
		return err
	}
	hash := TemplateHash(&d.Spec.Template)
	var active, current, outdated []*api.Pod
	oldTerminating := false
	for _, p := range dc.pods.List(ns, sel) {
		if ref := api.ControllerOf(&p.ObjectMeta); ref == nil || ref.UID != d.UID {
			continue
		}
		isCurrent := p.Labels[api.LabelTemplateHash] == hash
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
	dc.expect.expect(key, creates, uids)
	if err := dc.manage(ctx, d, hash, creates, deletes); err != nil {
		return err
	}
	return dc.updateStatus(ctx, d, active, current)
}

func (dc *DeploymentController) manage(ctx context.Context, d *api.Deployment, hash string, creates int, deletes []*api.Pod) error {
	key := informer.Key(d)
	var wg sync.WaitGroup
	errs := make(chan error, creates+len(deletes))
	for i := 0; i < creates; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := client.Pods(dc.client, d.Namespace).Create(ctx, podFromTemplate(d, hash))
			if err != nil {
				dc.expect.observeAdd(key)
				dc.recorder.Eventf(d, api.EventTypeWarning, "FailedCreate", "Error creating pod: %v", err)
				errs <- err
				return
			}
			dc.recorder.Eventf(d, api.EventTypeNormal, "SuccessfulCreate", "Created pod: %s", p.Name)
		}()
	}
	for _, p := range deletes {
		wg.Add(1)
		go func(p *api.Pod) {
			defer wg.Done()
			err := client.Pods(dc.client, p.Namespace).Delete(ctx, p.Name, &api.DeleteOptions{Preconditions: &api.Preconditions{UID: &p.UID}})
			if err != nil && !client.IsNotFound(err) {
				dc.expect.observeDelete(key, p.UID)
				errs <- err
				return
			}
			dc.recorder.Eventf(d, api.EventTypeNormal, "SuccessfulDelete", "Deleted pod: %s", p.Name)
		}(p)
	}
	wg.Wait()
	close(errs)
	return <-errs
}

func podFromTemplate(d *api.Deployment, hash string) *api.Pod {
	tpl := api.DeepCopy(&d.Spec.Template)
	labels := map[string]string{api.LabelTemplateHash: hash}
	for k, v := range tpl.Labels {
		labels[k] = v
	}
	return &api.Pod{
		ObjectMeta: api.ObjectMeta{
			GenerateName: d.Name + "-" + hash + "-", Namespace: d.Namespace,
			Labels: labels, Annotations: tpl.Annotations,
			OwnerReferences: []api.OwnerReference{api.NewControllerRef(d)},
		},
		Spec: tpl.Spec,
	}
}

// rankForDeletion orders Pods so the cheapest to lose come first:
// unscheduled, then pending, then not ready, then the newest.
func rankForDeletion(pods []*api.Pod) {
	score := func(p *api.Pod) int {
		switch {
		case p.Spec.NodeName == "":
			return 0
		case p.Status.Phase == api.PodPending:
			return 1
		case !api.IsPodReady(p):
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

func (dc *DeploymentController) updateStatus(ctx context.Context, d *api.Deployment, active, current []*api.Pod) error {
	ready := int32(0)
	for _, p := range active {
		if api.IsPodReady(p) {
			ready++
		}
	}
	want := *d.Spec.Replicas
	st := api.DeploymentStatus{
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
	cp := api.DeepCopy(d)
	cp.Status = st
	_, err := client.Deployments(dc.client, d.Namespace).UpdateStatus(ctx, cp)
	if client.IsConflict(err) || client.IsNotFound(err) {
		return nil // a newer version is already queued
	}
	return err
}
