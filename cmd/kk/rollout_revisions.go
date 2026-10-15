package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/controller/history"
)

// DaemonSetRolloutStatus describes a DaemonSet's rollout like `kubectl
// rollout status` and reports whether it is done.
func DaemonSetRolloutStatus(ds *appsv1.DaemonSet) (string, bool, error) {
	if ds.Spec.UpdateStrategy.Type != appsv1.RollingUpdateDaemonSetStrategyType {
		return "", true, errors.New("rollout status is only available for RollingUpdate strategy type")
	}
	if ds.Generation > ds.Status.ObservedGeneration {
		return "Waiting for daemon set spec update to be observed...", false, nil
	}
	st := ds.Status
	if st.UpdatedNumberScheduled < st.DesiredNumberScheduled {
		return fmt.Sprintf("Waiting for daemon set %q rollout to finish: %d out of %d new pods have been updated...",
			ds.Name, st.UpdatedNumberScheduled, st.DesiredNumberScheduled), false, nil
	}
	if st.NumberAvailable < st.DesiredNumberScheduled {
		return fmt.Sprintf("Waiting for daemon set %q rollout to finish: %d of %d updated pods are available...",
			ds.Name, st.NumberAvailable, st.DesiredNumberScheduled), false, nil
	}
	return fmt.Sprintf("daemon set %q successfully rolled out", ds.Name), true, nil
}

// StatefulSetRolloutStatus describes a StatefulSet's rollout like
// `kubectl rollout status` and reports whether it is done.
func StatefulSetRolloutStatus(s *appsv1.StatefulSet) (string, bool, error) {
	if s.Spec.UpdateStrategy.Type != appsv1.RollingUpdateStatefulSetStrategyType {
		return "", true, errors.New("rollout status is only available for RollingUpdate strategy type")
	}
	if s.Status.ObservedGeneration == 0 || s.Generation > s.Status.ObservedGeneration {
		return "Waiting for statefulset spec update to be observed...", false, nil
	}
	if r := s.Spec.Replicas; r != nil && s.Status.ReadyReplicas < *r {
		return fmt.Sprintf("Waiting for %d pods to be ready...", *r-s.Status.ReadyReplicas), false, nil
	}
	if ru := s.Spec.UpdateStrategy.RollingUpdate; ru != nil {
		if r := s.Spec.Replicas; r != nil && ru.Partition != nil && s.Status.UpdatedReplicas < *r-*ru.Partition {
			return fmt.Sprintf("Waiting for partitioned roll out to finish: %d out of %d new pods have been updated...",
				s.Status.UpdatedReplicas, *r-*ru.Partition), false, nil
		}
		return fmt.Sprintf("partitioned roll out complete: %d new pods have been updated...", s.Status.UpdatedReplicas), true, nil
	}
	if s.Status.UpdateRevision != s.Status.CurrentRevision {
		return fmt.Sprintf("waiting for statefulset rolling update to complete %d pods at revision %s...",
			s.Status.UpdatedReplicas, s.Status.UpdateRevision), false, nil
	}
	return fmt.Sprintf("statefulset rolling update complete %d pods at revision %s...", s.Status.CurrentReplicas, s.Status.CurrentRevision), true, nil
}

// revisioned is a controller whose history is kept in ControllerRevisions
// (a DaemonSet or a StatefulSet), for `kk rollout`.
type revisioned struct {
	kind   string // daemonset or statefulset
	name   string
	get    func(ctx context.Context) (metav1.Object, error)
	status func(ctx context.Context) (string, bool, error)
	patch  func(ctx context.Context, patch any) error
	// setTemplate writes a template back, retrying on conflicts.
	setTemplate func(ctx context.Context, tpl *corev1.PodTemplateSpec) error
}

func (a *app) daemonSetRevisioned(name string) revisioned {
	dss := client.DaemonSets(a.client, a.namespace)
	return revisioned{kind: "daemonset", name: name,
		get: func(ctx context.Context) (metav1.Object, error) { return dss.Get(ctx, name) },
		status: func(ctx context.Context) (string, bool, error) {
			ds, err := dss.Get(ctx, name)
			if err != nil {
				return "", false, err
			}
			return DaemonSetRolloutStatus(ds)
		},
		patch: func(ctx context.Context, patch any) error { _, err := dss.Patch(ctx, name, patch); return err },
		setTemplate: func(ctx context.Context, tpl *corev1.PodTemplateSpec) error {
			return retryOnConflict(func() error {
				ds, err := dss.Get(ctx, name)
				if err != nil {
					return err
				}
				ds.Spec.Template = *tpl
				_, err = dss.Update(ctx, ds)
				return err
			})
		},
	}
}

func (a *app) statefulSetRevisioned(name string) revisioned {
	sets := client.StatefulSets(a.client, a.namespace)
	return revisioned{kind: "statefulset", name: name,
		get: func(ctx context.Context) (metav1.Object, error) { return sets.Get(ctx, name) },
		status: func(ctx context.Context) (string, bool, error) {
			s, err := sets.Get(ctx, name)
			if err != nil {
				return "", false, err
			}
			return StatefulSetRolloutStatus(s)
		},
		patch: func(ctx context.Context, patch any) error { _, err := sets.Patch(ctx, name, patch); return err },
		setTemplate: func(ctx context.Context, tpl *corev1.PodTemplateSpec) error {
			return retryOnConflict(func() error {
				s, err := sets.Get(ctx, name)
				if err != nil {
					return err
				}
				s.Spec.Template = *tpl
				_, err = sets.Update(ctx, s)
				return err
			})
		},
	}
}

func retryOnConflict(f func() error) error {
	err := f()
	for attempt := 0; client.IsConflict(err) && attempt < 3; attempt++ {
		err = f()
	}
	return err
}

// revisionRollout implements `kk rollout` for DaemonSets and StatefulSets.
func (a *app) revisionRollout(ctx context.Context, verb string, w revisioned, p args) error {
	switch verb {
	case "status":
		return a.waitRollout(ctx, p.value("timeout", "0s"), func() (string, bool, error) { return w.status(ctx) })
	case "history":
		revisions, err := a.revisionHistory(ctx, w)
		if err != nil {
			return err
		}
		if r := p.value("revision", ""); r != "" {
			return a.printRevision(w, r, revisions)
		}
		fmt.Fprintf(a.out, "%s.apps/%s \n", w.kind, w.name)
		tw := newTabWriter(a.out)
		fmt.Fprintln(tw, "REVISION\tCHANGE-CAUSE")
		for _, cr := range revisions {
			fmt.Fprintf(tw, "%d\t%s\n", cr.Revision, orNone(cr.Annotations[appsv1.ChangeCauseAnnotation]))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.out)
		return err
	case "undo":
		return a.revisionUndo(ctx, w, p.value("to-revision", "0"))
	case "restart":
		patch := map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
			"annotations": map[string]any{appsv1.RestartedAtAnnotation: time.Now().Format(time.RFC3339)}}}}}
		if err := w.patch(ctx, patch); err != nil {
			return err
		}
		_, err := fmt.Fprintf(a.out, "%s.apps/%s restarted\n", w.kind, w.name)
		return err
	case "pause", "resume":
		verb := map[string]string{"pause": "pausing", "resume": "resuming"}[verb]
		return fmt.Errorf("%ss.apps %q %s is not supported", w.kind, w.name, verb)
	}
	return errors.New(rolloutUsage)
}

// revisionHistory returns the controller's revisions, oldest first.
func (a *app) revisionHistory(ctx context.Context, w revisioned) ([]appsv1.ControllerRevision, error) {
	obj, err := w.get(ctx)
	if err != nil {
		return nil, err
	}
	list, err := client.ControllerRevisions(a.client, a.namespace).List(ctx, client.ListOptions{})
	if err != nil {
		return nil, err
	}
	var owned []appsv1.ControllerRevision
	for _, cr := range list.Items {
		if ref := metav1.ControllerOf(&cr.ObjectMeta); ref != nil && ref.UID == obj.GetObjectMeta().UID {
			owned = append(owned, cr)
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].Revision < owned[j].Revision })
	return owned, nil
}

// printRevision shows the Pod template of one revision.
func (a *app) printRevision(w revisioned, revision string, revisions []appsv1.ControllerRevision) error {
	for i := range revisions {
		if strconv.FormatInt(revisions[i].Revision, 10) != revision {
			continue
		}
		tpl, err := history.RevisionTemplate(&revisions[i])
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "%s.apps/%s with revision #%s\nPod Template:\n", w.kind, w.name, revision)
		_, err = fmt.Fprint(a.out, indentYAML(tpl, "  "))
		return err
	}
	return fmt.Errorf("unable to find the specified revision")
}

// revisionUndo writes the template of an earlier revision back; the
// controller then makes that revision the newest.
func (a *app) revisionUndo(ctx context.Context, w revisioned, toRevision string) error {
	want, err := strconv.ParseInt(toRevision, 10, 64)
	if err != nil || want < 0 {
		return fmt.Errorf("invalid --to-revision %q", toRevision)
	}
	revisions, err := a.revisionHistory(ctx, w)
	if err != nil {
		return err
	}
	var target *appsv1.ControllerRevision
	switch {
	case want > 0:
		for i := range revisions {
			if revisions[i].Revision == want {
				target = &revisions[i]
			}
		}
		if target == nil {
			return fmt.Errorf("unable to find specified revision %d in history", want)
		}
	case len(revisions) < 2:
		return fmt.Errorf("no rollout history found for %s %q", w.kind, w.name)
	default:
		target = &revisions[len(revisions)-2] // the one before the newest
	}
	tpl, err := history.RevisionTemplate(target)
	if err != nil {
		return err
	}
	if err := w.setTemplate(ctx, tpl); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "%s.apps/%s rolled back\n", w.kind, w.name)
	return err
}
