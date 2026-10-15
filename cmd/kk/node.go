package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/scheme"
)

// cordon implements `kk cordon|uncordon NODE`.
func (a *app) cordon(ctx context.Context, p args, unschedulable bool) error {
	if len(p.positional) != 1 {
		return errors.New("usage: kk cordon|uncordon NODE")
	}
	verb := map[bool]string{true: "cordoned", false: "uncordoned"}[unschedulable]
	changed, err := a.setUnschedulable(ctx, p.positional[0], unschedulable)
	if err != nil {
		return err
	}
	if !changed {
		verb = "already " + verb
	}
	_, err = fmt.Fprintf(a.out, "node/%s %s\n", p.positional[0], verb)
	return err
}

func (a *app) setUnschedulable(ctx context.Context, name string, unschedulable bool) (bool, error) {
	node, err := client.Nodes(a.client).Get(ctx, name)
	if err != nil {
		return false, err
	}
	if node.Spec.Unschedulable == unschedulable {
		return false, nil
	}
	_, err = client.Nodes(a.client).Patch(ctx, name, map[string]any{"spec": map[string]any{"unschedulable": unschedulable}})
	return err == nil, err
}

// drain implements `kk drain NODE`, like kubectl drain: cordon the node,
// then evict its Pods (through the eviction subresource) and wait for them
// to go. Mirror Pods are left alone; Pods without a controller need
// --force and Pods with emptyDir volumes --delete-emptydir-data.
func (a *app) drain(ctx context.Context, p args) error {
	if len(p.positional) != 1 {
		return errors.New("usage: kk drain NODE [--force] [--delete-emptydir-data] [--ignore-daemonsets] [--grace-period N] [--timeout DURATION]")
	}
	name := p.positional[0]
	timeout, err := time.ParseDuration(p.value("timeout", "0s"))
	if err != nil {
		return fmt.Errorf("invalid --timeout %q", p.value("timeout", ""))
	}
	var grace *int64
	if v := p.value("grace-period", "-1"); v != "-1" {
		g, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid --grace-period %q", v)
		}
		grace = &g
	}
	if err := a.cordon(ctx, args{positional: []string{name}}, true); err != nil {
		return err
	}
	list, err := client.Pods(a.client, "").List(ctx, client.ListOptions{FieldSelector: "spec.nodeName=" + name})
	if err != nil {
		return err
	}
	var evict []corev1.Pod
	var unmanaged, local, daemons, problems []string
	for _, pod := range list.Items {
		ref := pod.Namespace + "/" + pod.Name
		owner := metav1.ControllerOf(&pod.ObjectMeta)
		switch {
		case corev1.IsMirrorPod(&pod):
			continue // the kubelet owns it
		case owner != nil && owner.Kind == "DaemonSet":
			daemons = append(daemons, ref)
			continue
		case owner == nil:
			unmanaged = append(unmanaged, ref)
		}
		if hasEmptyDir(&pod) {
			local = append(local, ref)
		}
		evict = append(evict, pod)
	}
	if len(daemons) > 0 && !p.boolean("ignore-daemonsets", false) {
		problems = append(problems, "cannot delete DaemonSet-managed Pods (use --ignore-daemonsets to ignore): "+strings.Join(daemons, ", "))
	}
	if len(unmanaged) > 0 && !p.boolean("force", false) {
		problems = append(problems, "cannot delete Pods declare no controller (use --force to override): "+strings.Join(unmanaged, ", "))
	}
	if len(local) > 0 && !p.boolean("delete-emptydir-data", false) {
		problems = append(problems, "cannot delete Pods with local storage (use --delete-emptydir-data to override): "+strings.Join(local, ", "))
	}
	if len(problems) > 0 {
		return fmt.Errorf("unable to drain node %q due to error: %s", name, strings.Join(problems, ", "))
	}
	if len(daemons) > 0 {
		fmt.Fprintf(a.errOut, "Warning: ignoring DaemonSet-managed Pods: %s\n", strings.Join(daemons, ", "))
	}
	if len(unmanaged) > 0 {
		fmt.Fprintf(a.errOut, "Warning: deleting Pods that declare no controller: %s\n", strings.Join(unmanaged, ", "))
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	for _, pod := range evict {
		fmt.Fprintf(a.out, "evicting pod %s/%s\n", pod.Namespace, pod.Name)
		body := map[string]any{"apiVersion": "policy/v1", "kind": "Eviction",
			"metadata": map[string]any{"name": pod.Name, "namespace": pod.Namespace}}
		if grace != nil {
			body["deleteOptions"] = map[string]any{"gracePeriodSeconds": *grace}
		}
		err := a.client.Do(ctx, http.MethodPost, scheme.Pods.Path(pod.Namespace, pod.Name, "eviction"), nil, "", body, nil)
		if err != nil && !client.IsNotFound(err) {
			return fmt.Errorf("evict %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}
	for _, pod := range evict {
		if err := a.waitPodGone(ctx, pod); err != nil {
			return fmt.Errorf("drain did not complete: %w", err)
		}
		fmt.Fprintf(a.out, "pod/%s evicted\n", pod.Name)
	}
	_, err = fmt.Fprintf(a.out, "node/%s drained\n", name)
	return err
}

func hasEmptyDir(p *corev1.Pod) bool {
	for _, v := range p.Spec.Volumes {
		if v.EmptyDir != nil {
			return true
		}
	}
	return false
}

// waitPodGone waits until the Pod (this instance, by UID) no longer exists.
func (a *app) waitPodGone(ctx context.Context, pod corev1.Pod) error {
	for {
		cur, err := client.Pods(a.client, pod.Namespace).Get(ctx, pod.Name)
		if client.IsNotFound(err) || (err == nil && cur.UID != pod.UID) {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("timed out waiting for pod %s/%s to be deleted", pod.Namespace, pod.Name)
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// taint implements `kk taint nodes NODE key[=value]:Effect ... | key[:Effect]- ...`.
func (a *app) taint(ctx context.Context, p args) error {
	pos := p.positional
	if len(pos) >= 1 && strings.Contains(pos[0], "/") {
		typ, name, _ := strings.Cut(pos[0], "/")
		pos = append([]string{typ, name}, pos[1:]...)
	}
	if len(pos) < 3 {
		return errors.New("usage: kk taint nodes NODE key[=value]:Effect ... | key[:Effect]- ...")
	}
	if res, err := lookupResource(pos[0]); err != nil || res.Resource != scheme.Nodes.Resource {
		return fmt.Errorf("taint only applies to nodes, not %q", pos[0])
	}
	name := pos[1]
	node, err := client.Nodes(a.client).Get(ctx, name)
	if err != nil {
		return err
	}
	taints := append([]corev1.Taint(nil), node.Spec.Taints...)
	added, removed := false, false
	for _, spec := range pos[2:] {
		if key, ok := strings.CutSuffix(spec, "-"); ok {
			key, effect, _ := strings.Cut(key, ":")
			kept := taints[:0]
			found := false
			for _, t := range taints {
				if t.Key == key && (effect == "" || t.Effect == effect) {
					found = true
					continue
				}
				kept = append(kept, t)
			}
			if !found {
				return fmt.Errorf("taint %q not found", spec[:len(spec)-1])
			}
			taints, removed = kept, true
			continue
		}
		kv, effect, ok := strings.Cut(spec, ":")
		if !ok || effect == "" {
			return fmt.Errorf("invalid taint spec: %s", spec)
		}
		key, value, _ := strings.Cut(kv, "=")
		replaced := false
		for i := range taints {
			if taints[i].Key == key && taints[i].Effect == effect {
				if !p.boolean("overwrite", false) {
					return fmt.Errorf("node %s already has %s taint(s) with same effect(s) and --overwrite is false", name, key)
				}
				taints[i].Value, replaced = value, true
			}
		}
		if !replaced {
			taints = append(taints, corev1.Taint{Key: key, Value: value, Effect: effect})
		}
		added = true
	}
	node.Spec.Taints = taints
	if _, err := client.Nodes(a.client).Update(ctx, node); err != nil {
		return err
	}
	verb := "tainted"
	switch {
	case added && removed:
		verb = "modified"
	case removed:
		verb = "untainted"
	}
	_, err = fmt.Fprintf(a.out, "node/%s %s\n", name, verb)
	return err
}
