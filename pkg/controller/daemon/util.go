package daemon

import (
	"sort"
	"time"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/deepcopy"
)

// daemonTolerations are added to every daemon Pod, as the DaemonSet
// controller does: it keeps running through node problems and on
// cordoned nodes.
var daemonTolerations = []corev1.Toleration{
	{Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	{Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	{Key: "node.kubernetes.io/disk-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	{Key: "node.kubernetes.io/memory-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	{Key: "node.kubernetes.io/pid-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	{Key: "node.kubernetes.io/unschedulable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
}

// networkUnavailable is tolerated by host-network daemon Pods.
var networkUnavailable = corev1.Toleration{Key: "node.kubernetes.io/network-unavailable", Operator: corev1.TolerationOpExists,
	Effect: corev1.TaintEffectNoSchedule}

// addTolerations adds the daemon tolerations the Pod does not have yet.
func addTolerations(spec *corev1.PodSpec) {
	extra := daemonTolerations
	if spec.HostNetwork {
		extra = append(append([]corev1.Toleration{}, extra...), networkUnavailable)
	}
	for _, t := range extra {
		found := false
		for _, have := range spec.Tolerations {
			found = found || (have.Key == t.Key && have.Effect == t.Effect)
		}
		if !found {
			spec.Tolerations = append(spec.Tolerations, t)
		}
	}
}

// NewPod builds the daemon Pod of the DaemonSet for a node: the template
// with the revision labels, the daemon tolerations and a required node
// affinity to that node (metadata.name), which the scheduler honours.
func NewPod(ds *appsv1.DaemonSet, node, hash string) *corev1.Pod {
	tpl := deepcopy.Copy(&ds.Spec.Template)
	labels := map[string]string{}
	for k, v := range tpl.Labels {
		labels[k] = v
	}
	if hash != "" {
		labels[appsv1.ControllerRevisionHashLabelKey] = hash
	}
	if g := ds.Annotations[appsv1.DeprecatedTemplateGeneration]; g != "" {
		labels[appsv1.DaemonSetTemplateGenerationKey] = g
	}
	spec := tpl.Spec
	addTolerations(&spec)
	if spec.Affinity == nil {
		spec.Affinity = &corev1.Affinity{}
	}
	if spec.Affinity.NodeAffinity == nil {
		spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	spec.Affinity.NodeAffinity.Required = &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}},
	}}}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: ds.Name + "-", Namespace: ds.Namespace, Labels: labels,
			Annotations: tpl.Annotations, OwnerReferences: []metav1.OwnerReference{metav1.NewControllerRef(ds)}},
		Spec: spec,
	}
}

// ShouldRunOn reports whether the DaemonSet wants a Pod on the node and
// whether a Pod already there may stay: the node must match the template's
// node selector and affinity; an untolerated NoSchedule taint keeps new
// Pods away, an untolerated NoExecute taint removes running ones too.
func ShouldRunOn(ds *appsv1.DaemonSet, node *corev1.Node) (shouldRun, shouldContinueRunning bool) {
	if node.DeletionTimestamp != nil {
		return false, false
	}
	spec := deepcopy.Copy(&ds.Spec.Template.Spec)
	addTolerations(spec)
	if !corev1.MatchesNodeSelector(spec, node) {
		return false, false
	}
	shouldRun = true
	for _, t := range node.Spec.Taints {
		if t.Effect == corev1.TaintEffectPreferNoSchedule {
			continue
		}
		tolerated := false
		for _, tol := range spec.Tolerations {
			tolerated = tolerated || tol.ToleratesTaint(t)
		}
		if tolerated {
			continue
		}
		if t.Effect == corev1.TaintEffectNoExecute {
			return false, false
		}
		shouldRun = false
	}
	return shouldRun, true
}

// podNode returns the node a daemon Pod is bound or pinned to.
func podNode(p *corev1.Pod) string {
	if p.Spec.NodeName != "" {
		return p.Spec.NodeName
	}
	if a := p.Spec.Affinity; a != nil && a.NodeAffinity != nil && a.NodeAffinity.Required != nil {
		for _, t := range a.NodeAffinity.Required.NodeSelectorTerms {
			for _, f := range t.MatchFields {
				if f.Key == "metadata.name" && f.Operator == corev1.NodeSelectorOpIn && len(f.Values) == 1 {
					return f.Values[0]
				}
			}
		}
	}
	return ""
}

// byAge orders Pods oldest first (by name for equal times).
func byAge(pods []*corev1.Pod) {
	sort.SliceStable(pods, func(i, j int) bool {
		a, b := pods[i].CreationTimestamp.Time, pods[j].CreationTimestamp.Time
		if !a.Equal(b) {
			return a.Before(b)
		}
		return pods[i].Name < pods[j].Name
	})
}

// nodePods are the live daemon Pods (not being deleted) of a node that
// should run the DaemonSet.
type nodePods struct {
	node string
	pods []*corev1.Pod
}

// splitByRevision returns the Pod of the current revision and the old one
// on a node; ok is false when there are several of either (the manage step
// removes duplicates first).
func splitByRevision(pods []*corev1.Pod, hash string) (newPod, oldPod *corev1.Pod, ok bool) {
	for _, p := range pods {
		if p.Labels[appsv1.ControllerRevisionHashLabelKey] == hash {
			if newPod != nil {
				return nil, nil, false
			}
			newPod = p
		} else {
			if oldPod != nil {
				return nil, nil, false
			}
			oldPod = p
		}
	}
	return newPod, oldPod, true
}

// planRollingUpdate decides the next step of a rolling update, like the
// DaemonSet controller's rollingUpdate: without surge, old Pods are deleted
// while fewer than maxUnavailable nodes lack an available Pod (unavailable
// old Pods go at once); with surge, new Pods start next to old ones on up
// to maxSurge nodes and the old ones go once the new ones are available.
func planRollingUpdate(nodes []nodePods, hash string, maxUnavailable, maxSurge int, available func(*corev1.Pod) bool) (
	deletes []*corev1.Pod, surgeNodes []string) {
	if maxSurge == 0 {
		numUnavailable := 0
		var allowed, candidates []*corev1.Pod
		for _, n := range nodes {
			newPod, oldPod, ok := splitByRevision(n.pods, hash)
			switch {
			case !ok || (oldPod == nil && newPod == nil) || (oldPod != nil && newPod != nil):
				numUnavailable++ // the manage step creates or deletes here
			case newPod != nil:
				if !available(newPod) {
					numUnavailable++
				}
			case !available(oldPod):
				allowed = append(allowed, oldPod)
			default:
				candidates = append(candidates, oldPod)
			}
		}
		remaining := min(max(maxUnavailable-numUnavailable, 0), len(candidates))
		return append(allowed, candidates[:remaining]...), nil
	}
	numSurge := 0
	var allowedNodes, candidateNodes []string
	for _, n := range nodes {
		newPod, oldPod, ok := splitByRevision(n.pods, hash)
		switch {
		case !ok:
			numSurge++
		case oldPod == nil:
		case newPod == nil:
			if !available(oldPod) {
				allowedNodes = append(allowedNodes, n.node)
			} else {
				candidateNodes = append(candidateNodes, n.node)
			}
		case !available(newPod):
			numSurge++
		default:
			deletes = append(deletes, oldPod)
		}
	}
	remaining := min(max(maxSurge-numSurge, 0), len(candidateNodes))
	return deletes, append(allowedNodes, candidateNodes[:remaining]...)
}

// isAvailable reports whether a Pod has been ready for minReadySeconds.
func isAvailable(p *corev1.Pod, minReadySeconds int32, now time.Time) bool {
	c := p.Status.Condition(corev1.PodReady)
	if c == nil || c.Status != corev1.ConditionTrue {
		return false
	}
	return minReadySeconds == 0 || now.Sub(c.LastTransitionTime.Time) >= time.Duration(minReadySeconds)*time.Second
}
