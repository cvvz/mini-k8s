// Package deployment implements the Deployment controller: like Kubernetes',
// it runs each version of a Deployment's Pod template as a ReplicaSet and
// moves replicas from old ReplicaSets to the new one according to the
// strategy (RollingUpdate or Recreate).
package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"time"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/client/workqueue"
	"mini-k8s/pkg/util/deepcopy"
)

// DeploymentController reconciles Deployments with their ReplicaSets.
type DeploymentController struct {
	client      *client.Client
	deployments *informers.Informer[appsv1.Deployment, *appsv1.Deployment]
	rsets       *informers.Informer[appsv1.ReplicaSet, *appsv1.ReplicaSet]
	pods        *informers.Informer[corev1.Pod, *corev1.Pod]
	recorder    *record.Recorder
	queue       *workqueue.Queue
	now         func() time.Time
}

// NewDeploymentController wires event handlers onto the shared informers.
func NewDeploymentController(c *client.Client, deployments *informers.Informer[appsv1.Deployment, *appsv1.Deployment],
	rsets *informers.Informer[appsv1.ReplicaSet, *appsv1.ReplicaSet], pods *informers.Informer[corev1.Pod, *corev1.Pod],
	recorder *record.Recorder) *DeploymentController {
	dc := &DeploymentController{client: c, deployments: deployments, rsets: rsets, pods: pods, recorder: recorder,
		queue: workqueue.New(), now: time.Now}
	enqueue := func(d *appsv1.Deployment) { dc.queue.Add(informers.Key(d)) }
	deployments.AddHandler(informers.Handler[*appsv1.Deployment]{
		OnAdd: enqueue, OnUpdate: func(_, d *appsv1.Deployment) { enqueue(d) }, OnDelete: enqueue,
	})
	onRS := func(rs *appsv1.ReplicaSet) {
		if ref := metav1.ControllerOf(&rs.ObjectMeta); ref != nil && ref.Kind == "Deployment" {
			dc.queue.Add(rs.Namespace + "/" + ref.Name)
		}
	}
	rsets.AddHandler(informers.Handler[*appsv1.ReplicaSet]{
		OnAdd: onRS, OnUpdate: func(_, rs *appsv1.ReplicaSet) { onRS(rs) }, OnDelete: onRS,
	})
	// Recreate waits for the old Pods to be gone.
	onPodGone := func(p *corev1.Pod) {
		ref := metav1.ControllerOf(&p.ObjectMeta)
		if ref == nil || ref.Kind != "ReplicaSet" {
			return
		}
		if rs, ok := rsets.Get(p.Namespace, ref.Name); ok {
			onRS(rs)
		}
	}
	pods.AddHandler(informers.Handler[*corev1.Pod]{OnDelete: onPodGone})
	return dc
}

// Run processes Deployments until ctx ends.
func (dc *DeploymentController) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, dc.queue, "deployment", workers, dc.sync)
}

// TemplateHash identifies a Pod template; it names the template's
// ReplicaSet and is stored in the pod-template-hash label of its Pods.
func TemplateHash(tpl *corev1.PodTemplateSpec) string {
	data, _ := json.Marshal(tpl)
	h := fnv.New32a()
	_, _ = h.Write(data)
	return safeEncode(fmt.Sprint(h.Sum32()))
}

// EqualIgnoreHash reports whether two Pod templates match apart from the
// pod-template-hash label once both carry today's defaults, like
// EqualIgnoreHash in Kubernetes' deployment utilities.
func EqualIgnoreHash(a, b *corev1.PodTemplateSpec) bool {
	canonical := func(t *corev1.PodTemplateSpec) []byte {
		t = deepcopy.Copy(t)
		delete(t.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
		corev1.SetDefaultsPodSpec(&t.Spec)
		data, _ := json.Marshal(t) // nil and empty maps encode alike
		return data
	}
	return bytes.Equal(canonical(a), canonical(b))
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
	cached, ok := dc.deployments.Get(ns, name)
	if !ok || cached.DeletionTimestamp != nil {
		return nil
	}
	// Objects stored before a field existed lack its default.
	d := deepcopy.Copy(cached)
	appsv1.SetDefaultsDeployment(d)
	selector, err := d.Spec.Selector.AsSelector()
	if err != nil {
		return err
	}
	hash := TemplateHash(&d.Spec.Template)
	var newRS *appsv1.ReplicaSet
	var old []*appsv1.ReplicaSet
	for _, cachedRS := range dc.rsets.List(ns, nil) {
		rs := deepcopy.Copy(cachedRS) // scaling updates it; the cache must stay untouched
		ref := metav1.ControllerOf(&rs.ObjectMeta)
		if ref == nil && rs.DeletionTimestamp == nil && selector.Matches(rs.Labels) {
			// An orphan (say, of a Deployment deleted with --cascade=orphan):
			// adopt it, as Kubernetes' ClaimReplicaSets does.
			if rs, err = dc.adopt(ctx, d, rs); err != nil {
				return err
			}
			ref = metav1.ControllerOf(&rs.ObjectMeta)
		}
		if ref == nil || ref.UID != d.UID {
			continue
		}
		old = append(old, rs)
	}
	byCreation(old)
	// The new ReplicaSet is the one whose template is the Deployment's: by
	// hash, or else by content, since a template stored by an older version
	// lacks newer defaults and hashes differently (FindNewReplicaSet).
	for _, rs := range old {
		if rs.Labels[appsv1.DefaultDeploymentUniqueLabelKey] == hash {
			newRS = rs
		} else if newRS == nil && EqualIgnoreHash(&rs.Spec.Template, &d.Spec.Template) {
			newRS = rs
		}
	}
	if newRS != nil {
		hash = newRS.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
		old = slices.DeleteFunc(old, func(rs *appsv1.ReplicaSet) bool { return rs == newRS })
	}
	s := &rollout{dc: dc, ctx: ctx, d: d, hash: hash, newRS: newRS, old: old}
	switch {
	case d.Spec.Paused:
		err = s.scaleWhilePaused()
	case d.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType:
		err = s.recreate()
	default:
		err = s.rollingUpdate()
	}
	if err != nil {
		return err
	}
	return s.syncStatus()
}

// adopt makes d the controller of an orphaned ReplicaSet; the
// resourceVersion guards against a concurrent change.
func (dc *DeploymentController) adopt(ctx context.Context, d *appsv1.Deployment, rs *appsv1.ReplicaSet) (*appsv1.ReplicaSet, error) {
	refs := append(append([]metav1.OwnerReference{}, rs.OwnerReferences...), metav1.NewControllerRef(d))
	adopted, err := client.ReplicaSets(dc.client, rs.Namespace).Patch(ctx, rs.Name, map[string]any{"metadata": map[string]any{
		"resourceVersion": rs.ResourceVersion, "ownerReferences": refs}})
	if err != nil {
		return nil, err
	}
	dc.recorder.Eventf(d, corev1.EventTypeNormal, "AdoptedReplicaSet", "Adopted replica set %s", rs.Name)
	return adopted, nil
}

// rollout is one sync of a Deployment.
type rollout struct {
	dc      *DeploymentController
	ctx     context.Context
	d       *appsv1.Deployment
	hash    string
	newRS   *appsv1.ReplicaSet
	old     []*appsv1.ReplicaSet
	created bool // the new ReplicaSet was created in this sync
}

func (s *rollout) all() []*appsv1.ReplicaSet {
	if s.newRS == nil {
		return s.old
	}
	return append([]*appsv1.ReplicaSet{s.newRS}, s.old...)
}

// ensureNewRS returns the ReplicaSet of the current template, creating it
// with the given size, and makes it the newest revision: an old
// ReplicaSet that becomes current again (a rollback) gets a new revision.
func (s *rollout) ensureNewRS(size int32) error {
	maxOld := int64(0)
	for _, rs := range s.old {
		maxOld = max(maxOld, Revision(rs))
	}
	revision := strconv.FormatInt(maxOld+1, 10)
	rsets := client.ReplicaSets(s.dc.client, s.d.Namespace)
	if s.newRS != nil {
		patch := map[string]any{}
		if Revision(s.newRS) <= maxOld {
			patch[appsv1.RevisionAnnotation] = revision // current again: a rollback
		}
		if cause, ok := s.d.Annotations[appsv1.ChangeCauseAnnotation]; ok && s.newRS.Annotations[appsv1.ChangeCauseAnnotation] != cause {
			patch[appsv1.ChangeCauseAnnotation] = cause
		}
		if len(patch) > 0 {
			updated, err := rsets.Patch(s.ctx, s.newRS.Name, map[string]any{"metadata": map[string]any{"annotations": patch}})
			if err != nil {
				return err
			}
			s.newRS = updated
		}
		return s.syncDeploymentRevision()
	}
	tpl := deepcopy.Copy(&s.d.Spec.Template)
	tpl.Labels = withHash(tpl.Labels, s.hash)
	selector := deepcopy.Copy(s.d.Spec.Selector)
	selector.MatchLabels = withHash(selector.MatchLabels, s.hash)
	annotations := map[string]string{appsv1.RevisionAnnotation: revision}
	if cause, ok := s.d.Annotations[appsv1.ChangeCauseAnnotation]; ok {
		annotations[appsv1.ChangeCauseAnnotation] = cause
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: s.d.Name + "-" + s.hash, Namespace: s.d.Namespace,
			Labels: tpl.Labels, Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{metav1.NewControllerRef(s.d)}},
		Spec: appsv1.ReplicaSetSpec{Replicas: &size, MinReadySeconds: s.d.Spec.MinReadySeconds, Selector: selector, Template: *tpl},
	}
	created, err := rsets.Create(s.ctx, rs)
	if client.IsAlreadyExists(err) {
		return fmt.Errorf("replica set %s exists but is not yet in the cache", rs.Name) // retried
	}
	if err != nil {
		return err
	}
	s.newRS, s.created = created, true
	if size > 0 {
		s.dc.recorder.Eventf(s.d, corev1.EventTypeNormal, "ScalingReplicaSet", "Scaled up replica set %s from 0 to %d", created.Name, size)
	}
	return s.syncDeploymentRevision()
}

func withHash(m map[string]string, hash string) map[string]string {
	out := map[string]string{appsv1.DefaultDeploymentUniqueLabelKey: hash}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// syncDeploymentRevision copies the new ReplicaSet's revision onto the
// Deployment.
func (s *rollout) syncDeploymentRevision() error {
	rev := s.newRS.Annotations[appsv1.RevisionAnnotation]
	if s.d.Annotations[appsv1.RevisionAnnotation] == rev {
		return nil
	}
	_, err := client.Deployments(s.dc.client, s.d.Namespace).Patch(s.ctx, s.d.Name,
		map[string]any{"metadata": map[string]any{"annotations": map[string]any{appsv1.RevisionAnnotation: rev}}})
	return err
}

// scale sets a ReplicaSet's size and records the change.
func (s *rollout) scale(rs *appsv1.ReplicaSet, size int32) error {
	from := replicas(rs)
	if from == size {
		return nil
	}
	updated, err := client.ReplicaSets(s.dc.client, rs.Namespace).Patch(s.ctx, rs.Name, map[string]any{"spec": map[string]any{"replicas": size}})
	if err != nil {
		return err
	}
	*rs = *updated
	verb := "up"
	if size < from {
		verb = "down"
	}
	s.dc.recorder.Eventf(s.d, corev1.EventTypeNormal, "ScalingReplicaSet", "Scaled %s replica set %s from %d to %d", verb, rs.Name, from, size)
	return nil
}

// rollingUpdate grows the new ReplicaSet within maxSurge and shrinks the old
// ones while keeping replicas - maxUnavailable Pods available.
func (s *rollout) rollingUpdate() error {
	if s.newRS == nil {
		if err := s.ensureNewRS(NewRSNewReplicas(s.d, s.old, nil)); err != nil {
			return err
		}
	} else if err := s.ensureNewRS(0); err != nil {
		return err
	}
	desired := *s.d.Spec.Replicas
	switch size := replicas(s.newRS); {
	case size > desired:
		return s.scale(s.newRS, desired)
	case size < desired:
		if grown := NewRSNewReplicas(s.d, s.all(), s.newRS); grown != size {
			return s.scale(s.newRS, grown) // the next sync scales the old ReplicaSets
		}
	}
	plan := OldScaleDown(s.d, s.newRS, s.old)
	for _, rs := range s.old {
		if size, ok := plan[rs.Name]; ok {
			if err := s.scale(rs, size); err != nil {
				return err
			}
		}
	}
	return s.cleanupHistory()
}

// recreate stops every old Pod before the new ReplicaSet starts.
func (s *rollout) recreate() error {
	scaledDown := false
	for _, rs := range s.old {
		if replicas(rs) > 0 {
			scaledDown = true
			if err := s.scale(rs, 0); err != nil {
				return err
			}
		}
	}
	if err := s.deleteLegacyPods(); err != nil {
		return err
	}
	if scaledDown || s.oldPodsExist() {
		return nil
	}
	if err := s.ensureNewRS(0); err != nil {
		return err
	}
	if err := s.scale(s.newRS, *s.d.Spec.Replicas); err != nil {
		return err
	}
	return s.cleanupHistory()
}

// deleteLegacyPods deletes Pods the Deployment controls directly: Pods
// created before Deployments managed ReplicaSets.
func (s *rollout) deleteLegacyPods() error {
	for _, p := range s.dc.pods.List(s.d.Namespace, nil) {
		ref := metav1.ControllerOf(&p.ObjectMeta)
		if ref == nil || ref.UID != s.d.UID || p.DeletionTimestamp != nil {
			continue
		}
		err := client.Pods(s.dc.client, p.Namespace).Delete(s.ctx, p.Name, &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &p.UID}})
		if err != nil && !client.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// oldPodsExist reports whether Pods of old ReplicaSets (or Pods the
// Deployment controls directly) remain, terminating ones included.
func (s *rollout) oldPodsExist() bool {
	owners := map[string]bool{s.d.UID: true}
	for _, rs := range s.old {
		owners[rs.UID] = true
	}
	for _, p := range s.dc.pods.List(s.d.Namespace, nil) {
		if ref := metav1.ControllerOf(&p.ObjectMeta); ref != nil && owners[ref.UID] && !corev1.IsPodTerminal(p) {
			return true
		}
	}
	return false
}

// scaleWhilePaused applies replica changes without rolling out the template:
// only when one ReplicaSet is active does it take the new size.
func (s *rollout) scaleWhilePaused() error {
	var active []*appsv1.ReplicaSet
	for _, rs := range s.all() {
		if replicas(rs) > 0 {
			active = append(active, rs)
		}
	}
	if len(active) == 1 {
		return s.scale(active[0], *s.d.Spec.Replicas)
	}
	return nil
}

// cleanupHistory deletes the oldest unused ReplicaSets beyond
// revisionHistoryLimit once the rollout is complete.
func (s *rollout) cleanupHistory() error {
	if s.newRS == nil || replicas(s.newRS) != *s.d.Spec.Replicas || s.newRS.Status.AvailableReplicas != *s.d.Spec.Replicas {
		return nil
	}
	// The new Pods are available: Pods from before ReplicaSets can go.
	if err := s.deleteLegacyPods(); err != nil {
		return err
	}
	var unused []*appsv1.ReplicaSet
	for _, rs := range s.old {
		if replicas(rs) == 0 && rs.Status.Replicas == 0 && rs.DeletionTimestamp == nil {
			unused = append(unused, rs)
		}
	}
	limit := int(*s.d.Spec.RevisionHistoryLimit)
	if len(unused) <= limit {
		return nil
	}
	sort.SliceStable(unused, func(i, j int) bool { return Revision(unused[i]) < Revision(unused[j]) })
	for _, rs := range unused[:len(unused)-limit] {
		err := client.ReplicaSets(s.dc.client, rs.Namespace).Delete(s.ctx, rs.Name, &metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &rs.UID}})
		if err != nil && !client.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// syncStatus reports replica counts and the Available and Progressing
// conditions with Kubernetes' reasons and messages.
func (s *rollout) syncStatus() error {
	d, now := s.d, metav1.NewTime(s.dc.now())
	st := appsv1.DeploymentStatus{ObservedGeneration: d.Generation}
	for _, rs := range s.all() {
		st.Replicas += rs.Status.Replicas
		st.ReadyReplicas += rs.Status.ReadyReplicas
		st.AvailableReplicas += rs.Status.AvailableReplicas
	}
	if s.newRS != nil {
		st.UpdatedReplicas = s.newRS.Status.Replicas
	}
	st.UnavailableReplicas = max(specReplicas(s.all())-st.AvailableReplicas, 0)
	st.Conditions = append(st.Conditions, d.Status.Conditions...)

	desired := *d.Spec.Replicas
	_, maxUnavailable := fenceposts(d)
	available := appsv1.DeploymentCondition{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue,
		Reason: "MinimumReplicasAvailable", Message: "Deployment has minimum availability."}
	if st.AvailableReplicas < desired-maxUnavailable {
		available.Status, available.Reason, available.Message = corev1.ConditionFalse, "MinimumReplicasUnavailable",
			"Deployment does not have minimum availability."
	}
	setCondition(&st, available, now)

	progressing, requeue := s.progressing(st, now)
	if progressing != nil {
		setCondition(&st, *progressing, now)
	}
	if requeue > 0 {
		s.dc.queue.AddAfter(informers.Key(d), requeue)
	}
	if reflect.DeepEqual(st, d.Status) {
		return nil
	}
	cp := deepcopy.Copy(d)
	cp.Status = st
	_, err := client.Deployments(s.dc.client, d.Namespace).UpdateStatus(s.ctx, cp)
	if client.IsConflict(err) || client.IsNotFound(err) {
		return nil // a newer version is already queued
	}
	return err
}

// progressing computes the Progressing condition and when to check again
// for the progress deadline.
func (s *rollout) progressing(st appsv1.DeploymentStatus, now metav1.Time) (*appsv1.DeploymentCondition, time.Duration) {
	d := s.d
	cond := func(status, reason, message string) *appsv1.DeploymentCondition {
		return &appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: status, Reason: reason, Message: message}
	}
	prev := d.Status.Condition(appsv1.DeploymentProgressing)
	if d.Spec.Paused {
		return cond(corev1.ConditionUnknown, "DeploymentPaused", "Deployment is paused"), 0
	}
	if prev != nil && prev.Reason == "DeploymentPaused" {
		return cond(corev1.ConditionUnknown, "DeploymentResumed", "Deployment is resumed"), 0
	}
	if s.newRS == nil {
		return nil, 0
	}
	name := s.newRS.Name
	desired := *d.Spec.Replicas
	if st.UpdatedReplicas == desired && st.Replicas == desired && st.AvailableReplicas == desired && replicas(s.newRS) == desired {
		return cond(corev1.ConditionTrue, "NewReplicaSetAvailable", fmt.Sprintf("ReplicaSet %q has successfully progressed.", name)), 0
	}
	old := d.Status
	progress := s.created || st.UpdatedReplicas > old.UpdatedReplicas || st.Replicas < old.Replicas ||
		st.ReadyReplicas > old.ReadyReplicas || st.AvailableReplicas > old.AvailableReplicas
	deadline := time.Duration(*d.Spec.ProgressDeadlineSeconds) * time.Second
	switch {
	case s.created:
		return cond(corev1.ConditionTrue, "NewReplicaSetCreated", fmt.Sprintf("Created new replica set %q", name)), deadline
	case progress || prev == nil || prev.Reason == "NewReplicaSetAvailable" || prev.Reason == "DeploymentResumed":
		return cond(corev1.ConditionTrue, "ReplicaSetUpdated", fmt.Sprintf("ReplicaSet %q is progressing.", name)), deadline
	case prev.Status == corev1.ConditionTrue:
		if elapsed := now.Sub(prev.LastUpdateTime.Time); elapsed >= deadline {
			return cond(corev1.ConditionFalse, "ProgressDeadlineExceeded", fmt.Sprintf("ReplicaSet %q has timed out progressing.", name)), 0
		} else {
			return nil, deadline - elapsed + time.Second
		}
	}
	return nil, 0
}

// setCondition sets c, refreshing LastUpdateTime when anything changed and
// LastTransitionTime when the status changed.
func setCondition(st *appsv1.DeploymentStatus, c appsv1.DeploymentCondition, now metav1.Time) {
	prev := st.Condition(c.Type)
	if prev == nil {
		c.LastUpdateTime, c.LastTransitionTime = now, now
		st.Conditions = append(st.Conditions, c)
		return
	}
	if prev.Status == c.Status && prev.Reason == c.Reason && prev.Message == c.Message && c.Reason != "ReplicaSetUpdated" {
		return
	}
	if prev.Status == c.Status && prev.Reason == c.Reason && prev.Message == c.Message {
		// Still progressing: only real progress refreshes the update time,
		// which the caller signals by asking for the condition again.
		c.LastTransitionTime, c.LastUpdateTime = prev.LastTransitionTime, now
		*prev = c
		return
	}
	c.LastUpdateTime, c.LastTransitionTime = now, now
	if prev.Status == c.Status {
		c.LastTransitionTime = prev.LastTransitionTime
	}
	*prev = c
}
