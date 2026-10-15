package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/scheme"
	"mini-k8s/pkg/util/deepcopy"
)

const rolloutUsage = `usage: kk rollout status|history|undo|pause|resume|restart deployment/NAME|daemonset/NAME
  status   [--timeout DURATION]   wait until the rollout finishes
  history  [--revision N]         list revisions and their change causes (or show one)
  undo     [--to-revision N]      roll back to the previous (or given) revision
  pause | resume                  stop or continue rolling out template changes (Deployments)
  restart                         replace every Pod with a fresh one`

// rollout implements `kk rollout`, like kubectl's.
func (a *app) rollout(ctx context.Context, p args) error {
	pos := p.positional
	if len(pos) == 2 && strings.Contains(pos[1], "/") {
		typ, name, _ := strings.Cut(pos[1], "/")
		pos = []string{pos[0], typ, name}
	}
	if len(pos) != 3 {
		return errors.New(rolloutUsage)
	}
	res, err := lookupResource(pos[1])
	if err != nil {
		return err
	}
	switch res.Resource {
	case scheme.DaemonSets.Resource:
		return a.revisionRollout(ctx, pos[0], a.daemonSetRevisioned(pos[2]), p)
	case scheme.StatefulSets.Resource:
		return a.revisionRollout(ctx, pos[0], a.statefulSetRevisioned(pos[2]), p)
	}
	if res.Resource != scheme.Deployments.Resource {
		return fmt.Errorf("rollout is not supported for %s", res.Resource)
	}
	name, deps := pos[2], client.Deployments(a.client, a.namespace)
	switch pos[0] {
	case "status":
		return a.waitRollout(ctx, p.value("timeout", "0s"), func() (string, bool, error) {
			d, err := deps.Get(ctx, name)
			if err != nil {
				return "", false, err
			}
			return RolloutStatus(d)
		})
	case "history":
		return a.rolloutHistory(ctx, name)
	case "undo":
		return a.rolloutUndo(ctx, name, p.value("to-revision", "0"))
	case "pause", "resume":
		if _, err := deps.Patch(ctx, name, map[string]any{"spec": map[string]any{"paused": pos[0] == "pause"}}); err != nil {
			return err
		}
		_, err := fmt.Fprintf(a.out, "deployment.apps/%s %s\n", name, map[string]string{"pause": "paused", "resume": "resumed"}[pos[0]])
		return err
	case "restart":
		patch := map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
			"annotations": map[string]any{appsv1.RestartedAtAnnotation: time.Now().Format(time.RFC3339)}}}}}
		if _, err := deps.Patch(ctx, name, patch); err != nil {
			return err
		}
		_, err := fmt.Fprintf(a.out, "deployment.apps/%s restarted\n", name)
		return err
	}
	return errors.New(rolloutUsage)
}

// RolloutStatus describes a Deployment's rollout like `kubectl rollout
// status` and reports whether it is done.
func RolloutStatus(d *appsv1.Deployment) (string, bool, error) {
	if d.Generation > d.Status.ObservedGeneration {
		return "Waiting for deployment spec update to be observed...", false, nil
	}
	if c := d.Status.Condition(appsv1.DeploymentProgressing); c != nil && c.Reason == "ProgressDeadlineExceeded" {
		return "", false, fmt.Errorf("deployment %q exceeded its progress deadline", d.Name)
	}
	st := d.Status
	switch {
	case st.UpdatedReplicas < *d.Spec.Replicas:
		return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d out of %d new replicas have been updated...", d.Name, st.UpdatedReplicas, *d.Spec.Replicas), false, nil
	case st.Replicas > st.UpdatedReplicas:
		return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d old replicas are pending termination...", d.Name, st.Replicas-st.UpdatedReplicas), false, nil
	case st.AvailableReplicas < st.UpdatedReplicas:
		return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d of %d updated replicas are available...", d.Name, st.AvailableReplicas, st.UpdatedReplicas), false, nil
	}
	return fmt.Sprintf("deployment %q successfully rolled out", d.Name), true, nil
}

// waitRollout prints the rollout status as it changes until it is done or
// the timeout passes.
func (a *app) waitRollout(ctx context.Context, timeout string, status func() (string, bool, error)) error {
	limit, err := time.ParseDuration(timeout)
	if err != nil {
		return fmt.Errorf("invalid --timeout %q", timeout)
	}
	if limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	last := ""
	for {
		msg, done, err := status()
		if err != nil {
			if ctx.Err() != nil {
				return errors.New("timed out waiting for the condition")
			}
			return err
		}
		if msg != last {
			if _, err := fmt.Fprintln(a.out, msg); err != nil {
				return err
			}
			last = msg
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("timed out waiting for the condition")
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// history returns the Deployment and its ReplicaSets, oldest revision first.
func (a *app) history(ctx context.Context, name string) (*appsv1.Deployment, []appsv1.ReplicaSet, error) {
	d, err := client.Deployments(a.client, a.namespace).Get(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	list, err := client.ReplicaSets(a.client, a.namespace).List(ctx, client.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	var owned []appsv1.ReplicaSet
	for _, rs := range list.Items {
		if ref := metav1.ControllerOf(&rs.ObjectMeta); ref != nil && ref.UID == d.UID {
			owned = append(owned, rs)
		}
	}
	revision := func(rs appsv1.ReplicaSet) int64 {
		n, _ := strconv.ParseInt(rs.Annotations[appsv1.RevisionAnnotation], 10, 64)
		return n
	}
	sort.Slice(owned, func(i, j int) bool { return revision(owned[i]) < revision(owned[j]) })
	return d, owned, nil
}

func (a *app) rolloutHistory(ctx context.Context, name string) error {
	_, rsets, err := a.history(ctx, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "deployment.apps/%s \n", name)
	tw := newTabWriter(a.out)
	fmt.Fprintln(tw, "REVISION\tCHANGE-CAUSE")
	for _, rs := range rsets {
		fmt.Fprintf(tw, "%s\t%s\n", rs.Annotations[appsv1.RevisionAnnotation], orNone(rs.Annotations[appsv1.ChangeCauseAnnotation]))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.out)
	return err
}

// rolloutUndo writes the template of an earlier revision back into the
// Deployment, which the Deployment controller then rolls out (reusing that
// revision's ReplicaSet).
func (a *app) rolloutUndo(ctx context.Context, name, toRevision string) error {
	want, err := strconv.ParseInt(toRevision, 10, 64)
	if err != nil || want < 0 {
		return fmt.Errorf("invalid --to-revision %q", toRevision)
	}
	d, rsets, err := a.history(ctx, name)
	if err != nil {
		return err
	}
	current := d.Annotations[appsv1.RevisionAnnotation]
	var target *appsv1.ReplicaSet
	for i := range rsets {
		rev := rsets[i].Annotations[appsv1.RevisionAnnotation]
		if (want == 0 && rev != current) || (want > 0 && rev == strconv.FormatInt(want, 10)) {
			target = &rsets[i] // with want 0: the newest revision before the current one
		}
	}
	if target == nil {
		if want > 0 {
			return fmt.Errorf("unable to find specified revision %d in history", want)
		}
		return fmt.Errorf("no rollout history found for deployment %q", name)
	}
	tpl := deepcopy.Copy(&target.Spec.Template)
	delete(tpl.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	revision := target.Annotations[appsv1.RevisionAnnotation]
	if reflect.DeepEqual(*tpl, d.Spec.Template) {
		_, err := fmt.Fprintf(a.out, "deployment.apps/%s skipped rollback (current template already matches revision %s)\n", name, revision)
		return err
	}
	for attempt := 0; ; attempt++ {
		d.Spec.Template = *tpl
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		if cause, ok := target.Annotations[appsv1.ChangeCauseAnnotation]; ok {
			d.Annotations[appsv1.ChangeCauseAnnotation] = cause
		} else {
			delete(d.Annotations, appsv1.ChangeCauseAnnotation)
		}
		_, err = client.Deployments(a.client, a.namespace).Update(ctx, d)
		if !client.IsConflict(err) || attempt == 3 {
			break
		}
		if d, err = client.Deployments(a.client, a.namespace).Get(ctx, name); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "deployment.apps/%s rolled back\n", name)
	return err
}

// replicaSetColumns are kubectl's columns for ReplicaSets.
func replicaSetColumns() []column {
	rs := func(o metav1.Object) *appsv1.ReplicaSet { return o.(*appsv1.ReplicaSet) }
	return []column{nameColumn(),
		{header: "DESIRED", value: func(o metav1.Object, _ time.Time) string { return fmt.Sprint(*rs(o).Spec.Replicas) }},
		{header: "CURRENT", value: func(o metav1.Object, _ time.Time) string { return fmt.Sprint(rs(o).Status.Replicas) }},
		{header: "READY", value: func(o metav1.Object, _ time.Time) string { return fmt.Sprint(rs(o).Status.ReadyReplicas) }},
		ageColumn(),
		{header: "CONTAINERS", wide: true, value: func(o metav1.Object, _ time.Time) string {
			return containerField(rs(o).Spec.Template.Spec.Containers, func(c corev1.Container) string { return c.Name })
		}},
		{header: "IMAGES", wide: true, value: func(o metav1.Object, _ time.Time) string {
			return containerField(rs(o).Spec.Template.Spec.Containers, func(c corev1.Container) string { return c.Image })
		}},
		{header: "SELECTOR", wide: true, value: func(o metav1.Object, _ time.Time) string {
			sel, _ := rs(o).Spec.Selector.AsSelector()
			return sel.String()
		}},
	}
}

func containerField(cs []corev1.Container, f func(corev1.Container) string) string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = f(c)
	}
	return strings.Join(out, ",")
}
