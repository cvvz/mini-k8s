// Package daemon implements the DaemonSet controller, like
// k8s.io/kubernetes/pkg/controller/daemon: it keeps one Pod of a DaemonSet
// on every node that should run it (the scheduler binds them to their
// node), replaces Pods after a template change according to the update
// strategy and records every template version as a ControllerRevision.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
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
	"mini-k8s/pkg/util/deepcopy"
	"mini-k8s/pkg/util/intstr"
	"mini-k8s/pkg/util/ptr"
)

// Failed daemon Pods are replaced after a backoff of 1s doubling to 5m.
const (
	failedBackoffInitial = time.Second
	failedBackoffMax     = 5 * time.Minute
)

// Controller reconciles DaemonSets with their Pods and revisions.
type Controller struct {
	client    *client.Client
	dsets     *informers.Informer[appsv1.DaemonSet, *appsv1.DaemonSet]
	pods      *informers.Informer[corev1.Pod, *corev1.Pod]
	nodes     *informers.Informer[corev1.Node, *corev1.Node]
	revisions *informers.Informer[appsv1.ControllerRevision, *appsv1.ControllerRevision]
	recorder  *record.Recorder
	queue     *workqueue.Queue
	expect    *controller.Expectations
	now       func() time.Time

	mu sync.Mutex
	// failed counts the failed Pods replaced per DaemonSet and node.
	failed map[string]failedPods
}

type failedPods struct {
	count int
	next  time.Time
}

// NewController wires the controller onto shared informers.
func NewController(c *client.Client, dsets *informers.Informer[appsv1.DaemonSet, *appsv1.DaemonSet],
	pods *informers.Informer[corev1.Pod, *corev1.Pod], nodes *informers.Informer[corev1.Node, *corev1.Node],
	revisions *informers.Informer[appsv1.ControllerRevision, *appsv1.ControllerRevision], recorder *record.Recorder) *Controller {
	dc := &Controller{client: c, dsets: dsets, pods: pods, nodes: nodes, revisions: revisions, recorder: recorder,
		queue: workqueue.New(), expect: controller.NewExpectations(), now: time.Now, failed: map[string]failedPods{}}
	enqueue := func(ds *appsv1.DaemonSet) { dc.queue.Add(informers.Key(ds)) }
	dsets.AddHandler(informers.Handler[*appsv1.DaemonSet]{
		OnAdd: enqueue, OnUpdate: func(_, ds *appsv1.DaemonSet) { enqueue(ds) },
		OnDelete: func(ds *appsv1.DaemonSet) {
			dc.mu.Lock()
			for key := range dc.failed {
				if strings.HasPrefix(key, ds.UID+"/") {
					delete(dc.failed, key)
				}
			}
			dc.mu.Unlock()
			enqueue(ds)
		},
	})
	pods.AddHandler(informers.Handler[*corev1.Pod]{
		OnAdd: func(p *corev1.Pod) {
			if key := controller.OwnerKey(p, "DaemonSet"); key != "" {
				dc.expect.ObserveAdd(key)
				dc.queue.Add(key)
			}
		},
		OnUpdate: func(old, cur *corev1.Pod) {
			if key := controller.OwnerKey(cur, "DaemonSet"); key != "" {
				if cur.DeletionTimestamp != nil && old.DeletionTimestamp == nil {
					dc.expect.ObserveDelete(key, cur.UID)
				}
				dc.queue.Add(key)
			}
		},
		OnDelete: func(p *corev1.Pod) {
			if key := controller.OwnerKey(p, "DaemonSet"); key != "" {
				dc.expect.ObserveDelete(key, p.UID)
				dc.queue.Add(key)
			}
		},
	})
	all := func() {
		for _, ds := range dsets.List("", nil) {
			enqueue(ds)
		}
	}
	nodes.AddHandler(informers.Handler[*corev1.Node]{
		OnAdd: func(*corev1.Node) { all() },
		OnUpdate: func(old, cur *corev1.Node) {
			if !reflect.DeepEqual(old.Labels, cur.Labels) || !reflect.DeepEqual(old.Spec.Taints, cur.Spec.Taints) {
				all()
			}
		},
		OnDelete: func(*corev1.Node) { all() },
	})
	return dc
}

// Run processes DaemonSets until ctx ends.
func (dc *Controller) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, dc.queue, "daemonset", workers, dc.sync)
}

func (dc *Controller) sync(ctx context.Context, key string) error {
	ns, name := informers.SplitKey(key)
	cached, ok := dc.dsets.Get(ns, name)
	if !ok {
		dc.expect.Forget(key)
		return nil
	}
	if cached.DeletionTimestamp != nil || !dc.expect.Satisfied(key) {
		return nil
	}
	ds := deepcopy.Copy(cached)
	appsv1.SetDefaultsDaemonSet(ds) // objects stored before a field existed
	hash, err := dc.syncRevisions(ctx, ds)
	if err != nil {
		return err
	}
	nodes := dc.nodes.List("", nil)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	byNode, err := dc.podsByNode(ctx, ds)
	if err != nil {
		return err
	}
	creates, deletes := dc.manage(ds, nodes, byNode, hash)
	if len(creates) == 0 && len(deletes) == 0 && ds.Spec.UpdateStrategy.Type == appsv1.RollingUpdateDaemonSetStrategyType {
		creates, deletes = dc.rollingUpdate(ds, nodes, byNode, hash)
	}
	err = dc.syncNodes(ctx, ds, creates, deletes, hash)
	if serr := dc.updateStatus(ctx, ds, nodes, byNode, hash); err == nil {
		err = serr
	}
	return err
}

// podsByNode groups the DaemonSet's Pods by the node they are pinned to,
// adopting orphaned Pods its selector matches (say, after the DaemonSet
// was deleted with --cascade=orphan and created again), as Kubernetes'
// ClaimPods does.
func (dc *Controller) podsByNode(ctx context.Context, ds *appsv1.DaemonSet) (map[string][]*corev1.Pod, error) {
	selector, err := ds.Spec.Selector.AsSelector()
	if err != nil {
		return nil, err
	}
	out := map[string][]*corev1.Pod{}
	for _, p := range dc.pods.List(ds.Namespace, nil) {
		ref := metav1.ControllerOf(&p.ObjectMeta)
		if ref == nil && p.DeletionTimestamp == nil && selector.Matches(p.Labels) {
			refs := append(append([]metav1.OwnerReference{}, p.OwnerReferences...), metav1.NewControllerRef(ds))
			adopted, err := client.Pods(dc.client, p.Namespace).Patch(ctx, p.Name, map[string]any{"metadata": map[string]any{
				"resourceVersion": p.ResourceVersion, "ownerReferences": refs}})
			if err != nil {
				return nil, err
			}
			p, ref = adopted, metav1.ControllerOf(&adopted.ObjectMeta)
		}
		if ref != nil && ref.UID == ds.UID {
			out[podNode(p)] = append(out[podNode(p)], p)
		}
	}
	for _, pods := range out {
		byAge(pods)
	}
	return out, nil
}

// surgeAllowed reports whether a node may run an old and a new Pod at
// once during a rolling update.
func surgeAllowed(ds *appsv1.DaemonSet, nodes int) bool {
	if ds.Spec.UpdateStrategy.Type != appsv1.RollingUpdateDaemonSetStrategyType || ds.Spec.UpdateStrategy.RollingUpdate == nil {
		return false
	}
	surge, _ := intstr.GetScaledValue(ds.Spec.UpdateStrategy.RollingUpdate.MaxSurge, nodes, true)
	return surge > 0
}

// manage makes every node that should run the DaemonSet have a Pod and
// the others none, and replaces failed Pods (after a backoff).
func (dc *Controller) manage(ds *appsv1.DaemonSet, nodes []*corev1.Node, byNode map[string][]*corev1.Pod, hash string) (
	creates []string, deletes []*corev1.Pod) {
	known := map[string]bool{}
	surge := surgeAllowed(ds, len(nodes))
	for _, node := range nodes {
		known[node.Name] = true
		shouldRun, shouldContinue := ShouldRunOn(ds, node)
		pods := byNode[node.Name]
		if !shouldContinue {
			for _, p := range pods {
				if p.DeletionTimestamp == nil {
					deletes = append(deletes, p)
				}
			}
			continue
		}
		if shouldRun && len(pods) == 0 {
			creates = append(creates, node.Name)
			continue
		}
		var live []*corev1.Pod
		for _, p := range pods {
			switch {
			case p.DeletionTimestamp != nil:
			case p.Status.Phase == corev1.PodFailed:
				if dc.failedPodMayGo(ds, node.Name) {
					dc.recorder.Eventf(ds, corev1.EventTypeWarning, "FailedDaemonPod",
						"Found failed daemon pod %s/%s on node %s, will try to kill it", p.Namespace, p.Name, node.Name)
					deletes = append(deletes, p)
				}
			default:
				live = append(live, p)
			}
		}
		// Keep the oldest Pod, or with surge the oldest of each revision
		// (an old and a new one while replacing).
		seen := map[bool]bool{}
		for i, p := range live {
			current := p.Labels[appsv1.ControllerRevisionHashLabelKey] == hash
			if (!surge && i > 0) || (surge && seen[current]) {
				deletes = append(deletes, p)
			}
			seen[current] = true
		}
	}
	// Pods pinned to nodes that are gone (the Pod garbage collector removes
	// bound ones; unbound ones would wait for the node forever).
	for node, pods := range byNode {
		if known[node] {
			continue
		}
		for _, p := range pods {
			if p.DeletionTimestamp == nil && p.Spec.NodeName == "" {
				deletes = append(deletes, p)
			}
		}
	}
	return creates, deletes
}

// failedPodMayGo paces the replacement of failed Pods on a node.
func (dc *Controller) failedPodMayGo(ds *appsv1.DaemonSet, node string) bool {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	key := ds.UID + "/" + node
	f := dc.failed[key]
	now := dc.now()
	if !f.next.IsZero() && now.Sub(f.next) > 2*failedBackoffMax {
		f = failedPods{} // quiet for long enough: start over
	}
	if now.Before(f.next) {
		dc.queue.AddAfter(informers.Key(ds), f.next.Sub(now))
		return false
	}
	f.count++
	f.next = now.Add(min(failedBackoffInitial<<min(f.count-1, 20), failedBackoffMax))
	dc.failed[key] = f
	return true
}

// rollingUpdate replaces Pods of old revisions on the nodes that should
// run the DaemonSet (see planRollingUpdate).
func (dc *Controller) rollingUpdate(ds *appsv1.DaemonSet, nodes []*corev1.Node, byNode map[string][]*corev1.Pod, hash string) (
	creates []string, deletes []*corev1.Pod) {
	var plan []nodePods
	for _, node := range nodes {
		if shouldRun, _ := ShouldRunOn(ds, node); !shouldRun {
			continue
		}
		var live []*corev1.Pod
		for _, p := range byNode[node.Name] {
			if p.DeletionTimestamp == nil {
				live = append(live, p)
			}
		}
		plan = append(plan, nodePods{node: node.Name, pods: live})
	}
	ru := ds.Spec.UpdateStrategy.RollingUpdate
	maxUnavailable, _ := intstr.GetScaledValue(ru.MaxUnavailable, len(plan), true)
	maxSurge, _ := intstr.GetScaledValue(ru.MaxSurge, len(plan), true)
	if maxUnavailable == 0 && maxSurge == 0 {
		maxUnavailable = 1
	}
	now := dc.now()
	deletes, creates = planRollingUpdate(plan, hash, maxUnavailable, maxSurge,
		func(p *corev1.Pod) bool { return isAvailable(p, ds.Spec.MinReadySeconds, now) })
	return creates, deletes
}

// syncNodes creates Pods for nodes and deletes Pods, recording what is
// expected so the next sync waits for the cache.
func (dc *Controller) syncNodes(ctx context.Context, ds *appsv1.DaemonSet, creates []string, deletes []*corev1.Pod, hash string) error {
	if len(creates) == 0 && len(deletes) == 0 {
		return nil
	}
	key := informers.Key(ds)
	uids := make([]string, len(deletes))
	for i, p := range deletes {
		uids[i] = p.UID
	}
	dc.expect.Expect(key, len(creates), uids)
	var wg sync.WaitGroup
	errs := make(chan error, len(creates)+len(deletes))
	for _, node := range creates {
		wg.Add(1)
		go func(node string) {
			defer wg.Done()
			p, err := client.Pods(dc.client, ds.Namespace).Create(ctx, NewPod(ds, node, hash))
			if err != nil {
				dc.expect.ObserveAdd(key)
				dc.recorder.Eventf(ds, corev1.EventTypeWarning, "FailedCreate", "Error creating: %v", err)
				errs <- err
				return
			}
			dc.recorder.Eventf(ds, corev1.EventTypeNormal, "SuccessfulCreate", "Created pod: %s", p.Name)
		}(node)
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
			dc.recorder.Eventf(ds, corev1.EventTypeNormal, "SuccessfulDelete", "Deleted pod: %s", p.Name)
		}(p)
	}
	wg.Wait()
	close(errs)
	return <-errs
}

// updateStatus counts the nodes that should run the DaemonSet and how far
// their Pods are; Pods that become available after minReadySeconds get
// the DaemonSet requeued.
func (dc *Controller) updateStatus(ctx context.Context, ds *appsv1.DaemonSet, nodes []*corev1.Node, byNode map[string][]*corev1.Pod, hash string) error {
	st := appsv1.DaemonSetStatus{ObservedGeneration: ds.Generation, CollisionCount: ds.Status.CollisionCount}
	now := dc.now()
	requeue := false
	for _, node := range nodes {
		shouldRun, _ := ShouldRunOn(ds, node)
		pods := byNode[node.Name]
		if !shouldRun {
			if len(pods) > 0 {
				st.NumberMisscheduled++
			}
			continue
		}
		st.DesiredNumberScheduled++
		if len(pods) == 0 {
			continue
		}
		st.CurrentNumberScheduled++
		// The node's oldest Pod counts, as in Kubernetes: during a surge the
		// old Pod, so a node counts as updated once only the new one is left.
		pod := pods[0]
		for _, p := range pods {
			if p.Spec.NodeName != "" {
				pod = p
				break
			}
		}
		if corev1.IsPodReady(pod) {
			st.NumberReady++
			if isAvailable(pod, ds.Spec.MinReadySeconds, now) {
				st.NumberAvailable++
			} else {
				requeue = true
			}
		}
		if pod.Labels[appsv1.ControllerRevisionHashLabelKey] == hash {
			st.UpdatedNumberScheduled++
		}
	}
	st.NumberUnavailable = st.DesiredNumberScheduled - st.NumberAvailable
	if requeue {
		dc.queue.AddAfter(informers.Key(ds), time.Second)
	}
	if reflect.DeepEqual(st, ds.Status) {
		return nil
	}
	cp := deepcopy.Copy(ds)
	cp.Status = st
	_, err := client.DaemonSets(dc.client, ds.Namespace).UpdateStatus(ctx, cp)
	if client.IsConflict(err) || client.IsNotFound(err) {
		return nil
	}
	return err
}

// RevisionData is a template as a ControllerRevision stores it: a patch
// replacing the controller's template.
func RevisionData(tpl *corev1.PodTemplateSpec) json.RawMessage {
	raw, _ := json.Marshal(tpl)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["$patch"] = "replace"
	data, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": m}})
	return data
}

// RevisionTemplate decodes the template of a ControllerRevision.
func RevisionTemplate(cr *appsv1.ControllerRevision) (*corev1.PodTemplateSpec, error) {
	var data struct {
		Spec struct {
			Template map[string]any `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(cr.Data, &data); err != nil {
		return nil, err
	}
	delete(data.Spec.Template, "$patch")
	raw, _ := json.Marshal(data.Spec.Template)
	var tpl corev1.PodTemplateSpec
	if err := json.Unmarshal(raw, &tpl); err != nil {
		return nil, err
	}
	return &tpl, nil
}

func sameTemplate(a, b *corev1.PodTemplateSpec) bool {
	canonical := func(t *corev1.PodTemplateSpec) []byte {
		t = deepcopy.Copy(t)
		corev1.SetDefaultsPodSpec(&t.Spec)
		data, _ := json.Marshal(t)
		return data
	}
	return bytes.Equal(canonical(a), canonical(b))
}

// syncRevisions finds or creates the ControllerRevision of the current
// template, makes it the newest (after a rollback to an older template),
// drops revisions beyond revisionHistoryLimit that no Pod uses and returns
// the current revision's hash.
func (dc *Controller) syncRevisions(ctx context.Context, ds *appsv1.DaemonSet) (string, error) {
	selector, err := ds.Spec.Selector.AsSelector()
	if err != nil {
		return "", err
	}
	revisions := client.ControllerRevisions(dc.client, ds.Namespace)
	var owned []*appsv1.ControllerRevision
	for _, cr := range dc.revisions.List(ds.Namespace, nil) {
		ref := metav1.ControllerOf(&cr.ObjectMeta)
		if ref == nil && cr.DeletionTimestamp == nil && selector.Matches(cr.Labels) {
			adopted, err := dc.adoptRevision(ctx, ds, cr)
			if err != nil {
				return "", err
			}
			cr, ref = adopted, metav1.ControllerOf(&adopted.ObjectMeta)
		}
		if ref != nil && ref.UID == ds.UID {
			owned = append(owned, cr)
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].Revision < owned[j].Revision })
	var maxRevision int64
	var cur *appsv1.ControllerRevision
	for _, cr := range owned {
		maxRevision = max(maxRevision, cr.Revision)
		if tpl, err := RevisionTemplate(cr); err == nil && sameTemplate(tpl, &ds.Spec.Template) {
			cur = cr
		}
	}
	switch {
	case cur == nil:
		var collisions int32
		if ds.Status.CollisionCount != nil {
			collisions = *ds.Status.CollisionCount
		}
		hash := controller.ComputeHashWithCollisions(&ds.Spec.Template, collisions)
		labels := map[string]string{appsv1.ControllerRevisionHashLabelKey: hash}
		for k, v := range ds.Spec.Template.Labels {
			labels[k] = v
		}
		annotations := map[string]string{appsv1.DeprecatedTemplateGeneration: ds.Annotations[appsv1.DeprecatedTemplateGeneration]}
		if cause := ds.Annotations[appsv1.ChangeCauseAnnotation]; cause != "" {
			annotations[appsv1.ChangeCauseAnnotation] = cause
		}
		cr := &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: ds.Name + "-" + hash, Namespace: ds.Namespace,
			Labels: labels, Annotations: annotations, OwnerReferences: []metav1.OwnerReference{metav1.NewControllerRef(ds)}},
			Data: RevisionData(&ds.Spec.Template), Revision: maxRevision + 1}
		created, err := revisions.Create(ctx, cr)
		if client.IsAlreadyExists(err) {
			// The name is taken by another template: count a collision so
			// the next attempt hashes differently, as Kubernetes does.
			cp := deepcopy.Copy(ds)
			cp.Status.CollisionCount = ptr.To(collisions + 1)
			if _, err := client.DaemonSets(dc.client, ds.Namespace).UpdateStatus(ctx, cp); err != nil && !client.IsConflict(err) {
				return "", err
			}
			return "", fmt.Errorf("controller revision %s exists with another template; collision count raised", cr.Name)
		}
		if err != nil {
			return "", err
		}
		cur = created
		owned = append(owned, created)
	case cur.Revision < maxRevision:
		cp := deepcopy.Copy(cur)
		cp.Revision = maxRevision + 1
		updated, err := revisions.Update(ctx, cp)
		if err != nil {
			return "", err
		}
		cur = updated
	}
	hash := cur.Labels[appsv1.ControllerRevisionHashLabelKey]
	return hash, dc.cleanupHistory(ctx, ds, owned, hash)
}

// adoptRevision makes the DaemonSet the controller of an orphaned
// revision of it.
func (dc *Controller) adoptRevision(ctx context.Context, ds *appsv1.DaemonSet, cr *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
	refs := append(append([]metav1.OwnerReference{}, cr.OwnerReferences...), metav1.NewControllerRef(ds))
	return client.ControllerRevisions(dc.client, cr.Namespace).Patch(ctx, cr.Name, map[string]any{"metadata": map[string]any{
		"resourceVersion": cr.ResourceVersion, "ownerReferences": refs}})
}

// cleanupHistory deletes the oldest revisions beyond the limit that are
// neither current nor used by a live Pod.
func (dc *Controller) cleanupHistory(ctx context.Context, ds *appsv1.DaemonSet, owned []*appsv1.ControllerRevision, hash string) error {
	limit := int(*ds.Spec.RevisionHistoryLimit)
	used := map[string]bool{hash: true}
	for _, p := range dc.pods.List(ds.Namespace, nil) {
		if ref := metav1.ControllerOf(&p.ObjectMeta); ref != nil && ref.UID == ds.UID {
			used[p.Labels[appsv1.ControllerRevisionHashLabelKey]] = true
		}
	}
	var unused []*appsv1.ControllerRevision
	for _, cr := range owned {
		if !used[cr.Labels[appsv1.ControllerRevisionHashLabelKey]] {
			unused = append(unused, cr)
		}
	}
	sort.Slice(unused, func(i, j int) bool { return unused[i].Revision < unused[j].Revision })
	for len(unused) > limit {
		err := client.ControllerRevisions(dc.client, ds.Namespace).Delete(ctx, unused[0].Name, nil)
		if err != nil && !client.IsNotFound(err) {
			return err
		}
		unused = unused[1:]
	}
	return nil
}
