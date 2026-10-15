// Package history keeps the ControllerRevisions of DaemonSets and
// StatefulSets, like k8s.io/kubernetes/pkg/controller/history: one
// revision per Pod template, numbered in the order the templates were
// used, so that a rollout knows which Pods are current and a rollback can
// restore an earlier template.
package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/controller"
	"mini-k8s/pkg/util/deepcopy"
)

// ErrCollision means the revision name of the template is taken by
// another template: the owner must raise its status.collisionCount so the
// next attempt hashes differently, as Kubernetes does.
var ErrCollision = errors.New("controller revision name collision")

// Owner describes the controller whose history is kept.
type Owner struct {
	// Object is the DaemonSet or StatefulSet (with its TypeMeta set).
	Object   metav1.Object
	Selector *metav1.LabelSelector
	Template *corev1.PodTemplateSpec
	// HashLabel is the label that holds the hash on revisions.
	HashLabel string
	// Annotations are copied onto a new revision.
	Annotations    map[string]string
	CollisionCount *int32
	// Limit is the number of old revisions kept besides those in use.
	Limit int
	// InUse tells revisions that must be kept (those live Pods run).
	InUse func(*appsv1.ControllerRevision) bool
}

// History reads revisions from a shared informer and writes them through
// the API.
type History struct {
	Client    *client.Client
	Revisions *informers.Informer[appsv1.ControllerRevision, *appsv1.ControllerRevision]
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

// Owned lists the owner's revisions, oldest first, adopting orphans its
// selector matches.
func (h *History) Owned(ctx context.Context, o Owner) ([]*appsv1.ControllerRevision, error) {
	selector, err := o.Selector.AsSelector()
	if err != nil {
		return nil, err
	}
	m := o.Object.GetObjectMeta()
	var owned []*appsv1.ControllerRevision
	for _, cr := range h.Revisions.List(m.Namespace, nil) {
		ref := metav1.ControllerOf(&cr.ObjectMeta)
		if ref == nil && cr.DeletionTimestamp == nil && selector.Matches(cr.Labels) {
			refs := append(append([]metav1.OwnerReference{}, cr.OwnerReferences...), metav1.NewControllerRef(o.Object))
			adopted, err := client.ControllerRevisions(h.Client, cr.Namespace).Patch(ctx, cr.Name, map[string]any{"metadata": map[string]any{
				"resourceVersion": cr.ResourceVersion, "ownerReferences": refs}})
			if err != nil {
				return nil, err
			}
			cr, ref = adopted, metav1.ControllerOf(&adopted.ObjectMeta)
		}
		if ref != nil && ref.UID == m.UID {
			owned = append(owned, cr)
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].Revision < owned[j].Revision })
	return owned, nil
}

// Sync finds or creates the revision of the owner's template and makes it
// the newest (after a rollback to an older template), then deletes the
// oldest revisions beyond the limit that are not in use. It returns that
// revision and every revision kept, oldest first.
func (h *History) Sync(ctx context.Context, o Owner) (*appsv1.ControllerRevision, []*appsv1.ControllerRevision, error) {
	owned, err := h.Owned(ctx, o)
	if err != nil {
		return nil, nil, err
	}
	m := o.Object.GetObjectMeta()
	revisions := client.ControllerRevisions(h.Client, m.Namespace)
	var maxRevision int64
	var cur *appsv1.ControllerRevision
	for _, cr := range owned {
		maxRevision = max(maxRevision, cr.Revision)
		if tpl, err := RevisionTemplate(cr); err == nil && sameTemplate(tpl, o.Template) {
			cur = cr
		}
	}
	if cur == nil {
		var collisions int32
		if o.CollisionCount != nil {
			collisions = *o.CollisionCount
		}
		hash := controller.ComputeHashWithCollisions(o.Template, collisions)
		labels := map[string]string{o.HashLabel: hash}
		for k, v := range o.Template.Labels {
			labels[k] = v
		}
		cr := &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: m.Name + "-" + hash, Namespace: m.Namespace,
			Labels: labels, Annotations: o.Annotations, OwnerReferences: []metav1.OwnerReference{metav1.NewControllerRef(o.Object)}},
			Data: RevisionData(o.Template), Revision: maxRevision + 1}
		created, err := revisions.Create(ctx, cr)
		if client.IsAlreadyExists(err) {
			created, err = h.reuse(ctx, o, cr)
		}
		if err != nil {
			return nil, nil, err
		}
		cur = created
		owned = append(owned, created)
	}
	newest := true
	for _, cr := range owned {
		newest = newest && (cr.UID == cur.UID || cr.Revision < cur.Revision)
	}
	if !newest {
		cp := deepcopy.Copy(cur)
		cp.Revision = maxRevision + 1
		updated, err := revisions.Update(ctx, cp)
		if err != nil {
			return nil, nil, err
		}
		for i := range owned {
			if owned[i].UID == updated.UID {
				owned[i] = updated
			}
		}
		cur = updated
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].Revision < owned[j].Revision })
	kept, err := h.trim(ctx, o, owned, cur)
	return cur, kept, err
}

// reuse handles a revision name that is taken. A revision of the same
// template is the owner's to take (an orphan, say, left by a set deleted
// with --cascade=orphan and created again): it is adopted, or waited for
// while the garbage collector still detaches it from its old owner. A
// revision of another template is a hash collision.
func (h *History) reuse(ctx context.Context, o Owner, want *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
	revisions := client.ControllerRevisions(h.Client, want.Namespace)
	existing, err := revisions.Get(ctx, want.Name)
	if err != nil {
		return nil, err
	}
	if tpl, terr := RevisionTemplate(existing); terr != nil || !sameTemplate(tpl, o.Template) {
		return nil, fmt.Errorf("%w: %s exists with another template", ErrCollision, want.Name)
	}
	ref := metav1.ControllerOf(&existing.ObjectMeta)
	switch {
	case ref != nil && ref.UID == o.Object.GetObjectMeta().UID:
		return existing, nil
	case ref != nil:
		return nil, fmt.Errorf("revision %s still belongs to %s %s", want.Name, ref.Kind, ref.UID)
	}
	refs := append(append([]metav1.OwnerReference{}, existing.OwnerReferences...), metav1.NewControllerRef(o.Object))
	return revisions.Patch(ctx, existing.Name, map[string]any{"metadata": map[string]any{
		"resourceVersion": existing.ResourceVersion, "ownerReferences": refs}})
}

// trim deletes the oldest revisions beyond the limit that are neither
// current nor in use.
func (h *History) trim(ctx context.Context, o Owner, owned []*appsv1.ControllerRevision, cur *appsv1.ControllerRevision) (
	[]*appsv1.ControllerRevision, error) {
	var unused []*appsv1.ControllerRevision
	for _, cr := range owned {
		if cr.UID != cur.UID && (o.InUse == nil || !o.InUse(cr)) {
			unused = append(unused, cr)
		}
	}
	gone := map[string]bool{}
	for len(unused) > o.Limit {
		err := client.ControllerRevisions(h.Client, unused[0].Namespace).Delete(ctx, unused[0].Name, nil)
		if err != nil && !client.IsNotFound(err) {
			return nil, err
		}
		gone[unused[0].UID] = true
		unused = unused[1:]
	}
	var kept []*appsv1.ControllerRevision
	for _, cr := range owned {
		if !gone[cr.UID] {
			kept = append(kept, cr)
		}
	}
	return kept, nil
}
