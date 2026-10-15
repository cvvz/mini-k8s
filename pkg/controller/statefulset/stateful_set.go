// Package statefulset implements the StatefulSet controller, like
// k8s.io/kubernetes/pkg/controller/statefulset: Pods <set>-<ordinal> with
// stable host names and one claim per volume template, created in order
// (or in parallel), removed from the highest ordinal down, and replaced
// from the highest ordinal down to the partition on a template change.
package statefulset

import (
	"context"
	"errors"
	"fmt"
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
	"mini-k8s/pkg/controller/history"
	"mini-k8s/pkg/controller/replicaset"
	"mini-k8s/pkg/util/deepcopy"
	"mini-k8s/pkg/util/ptr"
)

// errStaleClaim delays a Pod whose claim is still being deleted.
var errStaleClaim = errors.New("claim is being deleted")

// Controller reconciles StatefulSets with their Pods, claims and
// revisions.
type Controller struct {
	client   *client.Client
	sets     *informers.Informer[appsv1.StatefulSet, *appsv1.StatefulSet]
	pods     *informers.Informer[corev1.Pod, *corev1.Pod]
	claims   *informers.Informer[corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim]
	history  *history.History
	recorder *record.Recorder
	queue    *workqueue.Queue
	expect   *controller.Expectations
	now      func() time.Time
}

// NewController wires the controller onto shared informers.
func NewController(c *client.Client, sets *informers.Informer[appsv1.StatefulSet, *appsv1.StatefulSet],
	pods *informers.Informer[corev1.Pod, *corev1.Pod],
	claims *informers.Informer[corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim],
	revisions *informers.Informer[appsv1.ControllerRevision, *appsv1.ControllerRevision], recorder *record.Recorder) *Controller {
	sc := &Controller{client: c, sets: sets, pods: pods, claims: claims, history: &history.History{Client: c, Revisions: revisions},
		recorder: recorder, queue: workqueue.New(), expect: controller.NewExpectations(), now: time.Now}
	enqueue := func(s *appsv1.StatefulSet) { sc.queue.Add(informers.Key(s)) }
	sets.AddHandler(informers.Handler[*appsv1.StatefulSet]{
		OnAdd: enqueue, OnUpdate: func(_, s *appsv1.StatefulSet) { enqueue(s) }, OnDelete: enqueue,
	})
	// Pods without a controller may be adopted by a set that selects them.
	enqueueSelecting := func(ns string, labels map[string]string) {
		for _, s := range sets.List(ns, nil) {
			if sel, err := s.Spec.Selector.AsSelector(); err == nil && sel.Matches(labels) {
				enqueue(s)
			}
		}
	}
	podChanged := func(p *corev1.Pod, deleted bool) {
		key := controller.OwnerKey(p, "StatefulSet")
		switch {
		case key == "" && metav1.ControllerOf(&p.ObjectMeta) == nil && !deleted:
			enqueueSelecting(p.Namespace, p.Labels)
		case key != "":
			sc.queue.Add(key)
		}
	}
	pods.AddHandler(informers.Handler[*corev1.Pod]{
		OnAdd: func(p *corev1.Pod) {
			if key := controller.OwnerKey(p, "StatefulSet"); key != "" {
				sc.expect.ObserveAdd(key)
			}
			podChanged(p, false)
		},
		OnUpdate: func(old, cur *corev1.Pod) {
			if key := controller.OwnerKey(cur, "StatefulSet"); key != "" && cur.DeletionTimestamp != nil && old.DeletionTimestamp == nil {
				sc.expect.ObserveDelete(key, cur.UID)
			}
			podChanged(cur, false)
		},
		OnDelete: func(p *corev1.Pod) {
			if key := controller.OwnerKey(p, "StatefulSet"); key != "" {
				sc.expect.ObserveDelete(key, p.UID)
			}
			podChanged(p, true)
		},
	})
	// A claim that finished deleting unblocks its Pod.
	claims.AddHandler(informers.Handler[*corev1.PersistentVolumeClaim]{
		OnDelete: func(c *corev1.PersistentVolumeClaim) { enqueueSelecting(c.Namespace, c.Labels) },
	})
	return sc
}

// Run processes StatefulSets until ctx ends.
func (sc *Controller) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, sc.queue, "statefulset", workers, sc.sync)
}

func (sc *Controller) sync(ctx context.Context, key string) error {
	ns, name := informers.SplitKey(key)
	cached, ok := sc.sets.Get(ns, name)
	if !ok {
		sc.expect.Forget(key)
		return nil
	}
	if !sc.expect.Satisfied(key) {
		return nil
	}
	set := deepcopy.Copy(cached)
	appsv1.SetDefaultsStatefulSet(set)
	pods, err := sc.claimPods(ctx, set)
	if err != nil {
		return err
	}
	current, update, err := sc.revisions(ctx, set, pods)
	if err != nil {
		return err
	}
	var currentSet *appsv1.StatefulSet
	if tpl, terr := history.RevisionTemplate(current); terr == nil {
		currentSet = deepcopy.Copy(set)
		currentSet.Spec.Template = *tpl
	} else {
		currentSet = set
	}
	err = sc.update(ctx, set, currentSet, current.Name, update.Name, pods)
	status := sc.computeStatus(set, current.Name, update.Name, pods)
	if !reflect.DeepEqual(status, cached.Status) {
		cp := deepcopy.Copy(cached)
		cp.Status = status
		if _, uerr := client.StatefulSets(sc.client, ns).UpdateStatus(ctx, cp); uerr != nil && !client.IsNotFound(uerr) &&
			!client.IsConflict(uerr) && err == nil {
			err = uerr
		}
	}
	return err
}

// claimPods returns the set's Pods, adopting orphans its selector matches
// (after `kk delete statefulset --cascade=orphan` and a new set, say).
func (sc *Controller) claimPods(ctx context.Context, set *appsv1.StatefulSet) ([]*corev1.Pod, error) {
	selector, err := set.Spec.Selector.AsSelector()
	if err != nil {
		return nil, err
	}
	var out []*corev1.Pod
	for _, p := range sc.pods.List(set.Namespace, nil) {
		ref := metav1.ControllerOf(&p.ObjectMeta)
		switch {
		case ref != nil && ref.UID == set.UID:
			out = append(out, p)
		case ref == nil && p.DeletionTimestamp == nil && selector.Matches(p.Labels) && getOrdinal(set, p) >= 0 &&
			set.DeletionTimestamp == nil:
			refs := append(append([]metav1.OwnerReference{}, p.OwnerReferences...), metav1.NewControllerRef(set))
			adopted, err := client.Pods(sc.client, p.Namespace).Patch(ctx, p.Name, map[string]any{"metadata": map[string]any{
				"resourceVersion": p.ResourceVersion, "ownerReferences": refs}})
			if err != nil {
				return nil, err
			}
			out = append(out, adopted)
		}
	}
	return out, nil
}

// revisions brings the history up to date and returns the current
// revision (the one status.currentRevision names, or the update revision
// for a new set) and the update revision (the template's).
func (sc *Controller) revisions(ctx context.Context, set *appsv1.StatefulSet, pods []*corev1.Pod) (
	current, update *appsv1.ControllerRevision, err error) {
	used := map[string]bool{set.Status.CurrentRevision: true}
	for _, p := range pods {
		used[podRevision(p)] = true
	}
	annotations := map[string]string{}
	for k, v := range set.Annotations {
		annotations[k] = v
	}
	update, owned, err := sc.history.Sync(ctx, history.Owner{Object: set, Selector: set.Spec.Selector, Template: &set.Spec.Template,
		HashLabel: appsv1.ControllerRevisionHashLabel, Annotations: annotations, CollisionCount: set.Status.CollisionCount,
		Limit: int(*set.Spec.RevisionHistoryLimit), InUse: func(cr *appsv1.ControllerRevision) bool { return used[cr.Name] }})
	if errors.Is(err, history.ErrCollision) {
		cp := deepcopy.Copy(set)
		cp.Status.CollisionCount = ptr.To(ptr.Deref(set.Status.CollisionCount, 0) + 1)
		if _, uerr := client.StatefulSets(sc.client, set.Namespace).UpdateStatus(ctx, cp); uerr != nil && !client.IsConflict(uerr) {
			return nil, nil, uerr
		}
		return nil, nil, fmt.Errorf("%w; collision count raised", err)
	}
	if err != nil {
		return nil, nil, err
	}
	current = update
	for _, cr := range owned {
		if cr.Name == set.Status.CurrentRevision {
			current = cr
		}
	}
	return current, update, nil
}

// slot is an ordinal of the set and its Pod, if it exists.
type slot struct {
	ordinal int
	pod     *corev1.Pod
	created bool
}

// update makes the Pods match the set, as Kubernetes' updateStatefulSet:
// replace finished Pods and create missing ones (each after the previous
// one is healthy, unless Pods are managed in parallel), then remove Pods
// beyond the replicas from the highest ordinal down, then replace Pods of
// an old revision from the highest ordinal down to the partition.
func (sc *Controller) update(ctx context.Context, set, currentSet *appsv1.StatefulSet, current, update string, pods []*corev1.Pod) error {
	if set.DeletionTimestamp != nil {
		return nil
	}
	start, n := set.StartOrdinal(), int(*set.Spec.Replicas)
	replicas := make([]*slot, n)
	var condemned []*slot
	for _, p := range pods {
		ord := getOrdinal(set, p)
		switch {
		case ord < 0:
		case ord >= start && ord < start+n:
			replicas[ord-start] = &slot{ordinal: ord, pod: p, created: true}
		default:
			condemned = append(condemned, &slot{ordinal: ord, pod: p, created: true})
		}
	}
	partition := 0
	if ru := set.Spec.UpdateStrategy.RollingUpdate; set.Spec.UpdateStrategy.Type == appsv1.RollingUpdateStatefulSetStrategyType && ru != nil {
		partition = int(ptr.Deref(ru.Partition, 0))
	}
	for i := range replicas {
		if replicas[i] == nil {
			ord := start + i
			// Ordinals below the partition keep the current revision.
			pod := newPod(set, &set.Spec.Template, update, ord)
			if set.Spec.UpdateStrategy.Type == appsv1.RollingUpdateStatefulSetStrategyType && ord < start+partition {
				pod = newPod(set, &currentSet.Spec.Template, current, ord)
			}
			replicas[i] = &slot{ordinal: ord, pod: pod}
		}
	}
	sort.Slice(condemned, func(i, j int) bool { return condemned[i].ordinal > condemned[j].ordinal })
	monotonic := set.Spec.PodManagementPolicy != appsv1.ParallelPodManagement

	// Claims of condemned Pods follow them if the policy says so.
	for _, s := range condemned {
		if err := sc.fixClaimOwners(ctx, set, s.pod, s.ordinal, true); err != nil {
			return err
		}
	}
	var firstUnhealthy *corev1.Pod
	for _, s := range replicas {
		if (!s.created || !isHealthy(s.pod)) && firstUnhealthy == nil {
			firstUnhealthy = s.pod
		}
	}
	for i := len(condemned) - 1; i >= 0; i-- {
		if !isHealthy(condemned[i].pod) && firstUnhealthy == nil {
			firstUnhealthy = condemned[i].pod
		}
	}

	if stop, err := runForAll(replicas, monotonic, func(s *slot) (bool, error) { return sc.processReplica(ctx, set, s, monotonic) }); stop || err != nil {
		return err
	}
	if stop, err := runForAll(condemned, monotonic, func(s *slot) (bool, error) {
		return sc.processCondemned(ctx, set, s, monotonic, firstUnhealthy)
	}); stop || err != nil {
		return err
	}
	if set.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
		return nil
	}
	for target := len(replicas) - 1; target >= partition; target-- {
		s := replicas[target]
		if s.created && podRevision(s.pod) != update && !isTerminating(s.pod) {
			return sc.deletePod(ctx, set, s.pod)
		}
		if !s.created || !isHealthy(s.pod) {
			return nil // wait for the replaced Pod
		}
	}
	return nil
}

// runForAll applies fn to every slot: in order, stopping at the first that
// asks to, when monotonic; otherwise to all of them.
func runForAll(slots []*slot, monotonic bool, fn func(*slot) (bool, error)) (bool, error) {
	if monotonic {
		for _, s := range slots {
			if stop, err := fn(s); stop || err != nil {
				return true, err
			}
		}
		return false, nil
	}
	var wg sync.WaitGroup
	errs := make([]error, len(slots))
	for i, s := range slots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = fn(s)
		}()
	}
	wg.Wait()
	err := errors.Join(errs...)
	return err != nil, err
}

func (sc *Controller) processReplica(ctx context.Context, set *appsv1.StatefulSet, s *slot, monotonic bool) (bool, error) {
	if !s.created {
		if err := sc.createClaims(ctx, set, s.ordinal); err != nil {
			if errors.Is(err, errStaleClaim) {
				return true, nil // the claim's deletion will requeue the set
			}
			return true, err
		}
		if err := sc.createPod(ctx, set, s.pod); err != nil {
			return true, err
		}
		return monotonic, nil
	}
	p := s.pod
	// A finished Pod is deleted and comes back fresh, whatever its exit code.
	if isFinished(p) {
		if !isTerminating(p) {
			return true, sc.deletePod(ctx, set, p)
		}
		return true, nil
	}
	if p.Status.Phase == corev1.PodPending {
		// Claims deleted under a pending Pod are made again.
		if err := sc.createClaims(ctx, set, s.ordinal); err != nil && !errors.Is(err, errStaleClaim) {
			return true, err
		}
	}
	if monotonic {
		if isTerminating(p) || !isRunningAndReady(p) {
			return true, nil
		}
		if ok, wait := replicaset.IsPodAvailable(p, set.Spec.MinReadySeconds, sc.now()); !ok {
			sc.queue.AddAfter(informers.Key(set), wait)
			return true, nil
		}
	}
	if !identityMatches(set, p, s.ordinal) && !isTerminating(p) {
		patch := map[string]any{"metadata": map[string]any{"labels": map[string]any{
			appsv1.StatefulSetPodNameLabel: podName(set, s.ordinal), appsv1.PodIndexLabel: fmt.Sprint(s.ordinal)}}}
		if _, err := client.Pods(sc.client, p.Namespace).Patch(ctx, p.Name, patch); err != nil {
			return true, err
		}
	}
	return false, sc.fixClaimOwners(ctx, set, p, s.ordinal, false)
}

func (sc *Controller) processCondemned(ctx context.Context, set *appsv1.StatefulSet, s *slot, monotonic bool, firstUnhealthy *corev1.Pod) (
	bool, error) {
	p := s.pod
	if isTerminating(p) {
		return monotonic, nil // in order, wait for it to go
	}
	if monotonic && p != firstUnhealthy {
		if !isRunningAndReady(p) {
			return true, nil
		}
		if ok, _ := replicaset.IsPodAvailable(p, set.Spec.MinReadySeconds, sc.now()); !ok {
			return true, nil
		}
	}
	return true, sc.deletePod(ctx, set, p)
}

// createClaims creates the missing claims of an ordinal; a claim still
// being deleted (a scale down that is not over) blocks the Pod.
func (sc *Controller) createClaims(ctx context.Context, set *appsv1.StatefulSet, ordinal int) error {
	for i := range set.Spec.VolumeClaimTemplates {
		tpl := &set.Spec.VolumeClaimTemplates[i]
		name := claimName(set, tpl, ordinal)
		if c, ok := sc.claims.Get(set.Namespace, name); ok {
			if c.DeletionTimestamp != nil {
				return fmt.Errorf("%w: %s", errStaleClaim, name)
			}
			continue
		}
		claim := newClaim(set, tpl, ordinal)
		claim.OwnerReferences = claimOwnerRefs(set, nil, false, nil)
		_, err := client.PersistentVolumeClaims(sc.client, set.Namespace).Create(ctx, claim)
		switch {
		case client.IsAlreadyExists(err):
			// Made in an earlier sync the cache has not seen yet.
		case err != nil:
			sc.recorder.Eventf(set, corev1.EventTypeWarning, "FailedCreate", "Create Claim %s for Pod %s in StatefulSet %s failed error: %v",
				name, podName(set, ordinal), set.Name, err)
			return err
		default:
			sc.recorder.Eventf(set, corev1.EventTypeNormal, "SuccessfulCreate", "Create Claim %s Pod %s in StatefulSet %s success",
				name, podName(set, ordinal), set.Name)
		}
	}
	return nil
}

// fixClaimOwners makes the owner references of a Pod's claims follow the
// retention policy.
func (sc *Controller) fixClaimOwners(ctx context.Context, set *appsv1.StatefulSet, p *corev1.Pod, ordinal int, scaledDown bool) error {
	for i := range set.Spec.VolumeClaimTemplates {
		c, ok := sc.claims.Get(set.Namespace, claimName(set, &set.Spec.VolumeClaimTemplates[i], ordinal))
		if !ok || c.DeletionTimestamp != nil {
			continue
		}
		refs := claimOwnerRefs(set, p, scaledDown, c.OwnerReferences)
		if sameOwners(refs, c.OwnerReferences) {
			continue
		}
		if refs == nil {
			refs = []metav1.OwnerReference{}
		}
		_, err := client.PersistentVolumeClaims(sc.client, c.Namespace).Patch(ctx, c.Name, map[string]any{"metadata": map[string]any{
			"resourceVersion": c.ResourceVersion, "ownerReferences": refs}})
		if err != nil && !client.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (sc *Controller) createPod(ctx context.Context, set *appsv1.StatefulSet, p *corev1.Pod) error {
	key := informers.Key(set)
	sc.expect.Raise(key, 1, nil)
	_, err := client.Pods(sc.client, set.Namespace).Create(ctx, p)
	if err != nil {
		sc.expect.ObserveAdd(key)
		if client.IsAlreadyExists(err) {
			return nil // the cache is behind; the Pod shows up soon
		}
		sc.recorder.Eventf(set, corev1.EventTypeWarning, "FailedCreate", "Create Pod %s in StatefulSet %s failed error: %v", p.Name, set.Name, err)
		return err
	}
	sc.recorder.Eventf(set, corev1.EventTypeNormal, "SuccessfulCreate", "Create Pod %s in StatefulSet %s successful", p.Name, set.Name)
	return nil
}

func (sc *Controller) deletePod(ctx context.Context, set *appsv1.StatefulSet, p *corev1.Pod) error {
	key := informers.Key(set)
	sc.expect.Raise(key, 0, []string{p.UID})
	err := client.Pods(sc.client, p.Namespace).Delete(ctx, p.Name, &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &p.UID}})
	if err != nil && !client.IsNotFound(err) {
		sc.expect.ObserveDelete(key, p.UID)
		sc.recorder.Eventf(set, corev1.EventTypeWarning, "FailedDelete", "Delete Pod %s in StatefulSet %s failed error: %v", p.Name, set.Name, err)
		return err
	}
	if err == nil {
		sc.recorder.Eventf(set, corev1.EventTypeNormal, "SuccessfulDelete", "Delete Pod %s in StatefulSet %s successful", p.Name, set.Name)
	}
	return nil
}

// computeStatus counts the Pods as Kubernetes does; once every Pod runs
// the update revision and is ready, it becomes the current revision.
func (sc *Controller) computeStatus(set *appsv1.StatefulSet, current, update string, pods []*corev1.Pod) appsv1.StatefulSetStatus {
	st := appsv1.StatefulSetStatus{ObservedGeneration: set.Generation, CurrentRevision: current, UpdateRevision: update,
		CollisionCount: set.Status.CollisionCount}
	now := sc.now()
	for _, p := range pods {
		created := p.Status.Phase != ""
		if created {
			st.Replicas++
		}
		if isRunningAndReady(p) {
			st.ReadyReplicas++
			if ok, wait := replicaset.IsPodAvailable(p, set.Spec.MinReadySeconds, now); ok {
				st.AvailableReplicas++
			} else {
				sc.queue.AddAfter(informers.Key(set), wait) // count it once it is available
			}
		}
		if created && !isTerminating(p) {
			if podRevision(p) == current {
				st.CurrentReplicas++
			}
			if podRevision(p) == update {
				st.UpdatedReplicas++
			}
		}
	}
	if set.Spec.UpdateStrategy.Type == appsv1.RollingUpdateStatefulSetStrategyType && st.UpdatedReplicas == st.Replicas &&
		st.ReadyReplicas == st.Replicas {
		st.CurrentReplicas, st.CurrentRevision = st.UpdatedReplicas, st.UpdateRevision
	}
	return st
}
