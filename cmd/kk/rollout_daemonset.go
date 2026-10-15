package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/controller/daemon"
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

// daemonSetRollout implements `kk rollout` for DaemonSets.
func (a *app) daemonSetRollout(ctx context.Context, verb, name string, p args) error {
	dss := client.DaemonSets(a.client, a.namespace)
	switch verb {
	case "status":
		return a.waitRollout(ctx, p.value("timeout", "0s"), func() (string, bool, error) {
			ds, err := dss.Get(ctx, name)
			if err != nil {
				return "", false, err
			}
			return DaemonSetRolloutStatus(ds)
		})
	case "history":
		_, revisions, err := a.daemonSetHistory(ctx, name)
		if err != nil {
			return err
		}
		if r := p.value("revision", ""); r != "" {
			return a.printRevision(name, r, revisions)
		}
		fmt.Fprintf(a.out, "daemonset.apps/%s \n", name)
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
		return a.daemonSetUndo(ctx, name, p.value("to-revision", "0"))
	case "restart":
		patch := map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
			"annotations": map[string]any{appsv1.RestartedAtAnnotation: time.Now().Format(time.RFC3339)}}}}}
		if _, err := dss.Patch(ctx, name, patch); err != nil {
			return err
		}
		_, err := fmt.Fprintf(a.out, "daemonset.apps/%s restarted\n", name)
		return err
	case "pause", "resume":
		verb := map[string]string{"pause": "pausing", "resume": "resuming"}[verb]
		return fmt.Errorf("daemonsets.apps %q %s is not supported", name, verb)
	}
	return errors.New(rolloutUsage)
}

// daemonSetHistory returns the DaemonSet and its revisions, oldest first.
func (a *app) daemonSetHistory(ctx context.Context, name string) (*appsv1.DaemonSet, []appsv1.ControllerRevision, error) {
	ds, err := client.DaemonSets(a.client, a.namespace).Get(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	list, err := client.ControllerRevisions(a.client, a.namespace).List(ctx, client.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	var owned []appsv1.ControllerRevision
	for _, cr := range list.Items {
		if ref := metav1.ControllerOf(&cr.ObjectMeta); ref != nil && ref.UID == ds.UID {
			owned = append(owned, cr)
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].Revision < owned[j].Revision })
	return ds, owned, nil
}

// printRevision shows the Pod template of one revision.
func (a *app) printRevision(name, revision string, revisions []appsv1.ControllerRevision) error {
	for i := range revisions {
		if strconv.FormatInt(revisions[i].Revision, 10) != revision {
			continue
		}
		tpl, err := daemon.RevisionTemplate(&revisions[i])
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "daemonset.apps/%s with revision #%s\nPod Template:\n", name, revision)
		_, err = fmt.Fprint(a.out, indentYAML(tpl, "  "))
		return err
	}
	return fmt.Errorf("unable to find the specified revision")
}

// daemonSetUndo writes the template of an earlier revision back; the
// DaemonSet controller then makes that revision the newest.
func (a *app) daemonSetUndo(ctx context.Context, name, toRevision string) error {
	want, err := strconv.ParseInt(toRevision, 10, 64)
	if err != nil || want < 0 {
		return fmt.Errorf("invalid --to-revision %q", toRevision)
	}
	ds, revisions, err := a.daemonSetHistory(ctx, name)
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
		return fmt.Errorf("no rollout history found for daemonset %q", name)
	default:
		target = &revisions[len(revisions)-2] // the one before the newest
	}
	tpl, err := daemon.RevisionTemplate(target)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		ds.Spec.Template = *tpl
		_, err = client.DaemonSets(a.client, a.namespace).Update(ctx, ds)
		if !client.IsConflict(err) || attempt == 3 {
			break
		}
		if ds, err = client.DaemonSets(a.client, a.namespace).Get(ctx, name); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "daemonset.apps/%s rolled back\n", name)
	return err
}
