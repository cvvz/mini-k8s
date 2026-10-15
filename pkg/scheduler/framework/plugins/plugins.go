// Package plugins holds the default scheduling plugins, named and weighted
// like kube-scheduler's default profile.
package plugins

import (
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/scheduler/framework"
)

// Default returns the framework of the default profile.
func Default() *framework.Framework {
	return &framework.Framework{
		Filters: []framework.FilterPlugin{NodeUnschedulable{}, TaintToleration{}, NodeAffinity{}, NodeResourcesFit{}, InterPodAffinity{}},
		Scores:  []framework.ScorePlugin{TaintToleration{}, NodeAffinity{}, PodTopologySpread{}, InterPodAffinity{}, NodeResourcesFit{}},
	}
}

// Failure reasons, as kube-scheduler words them.
const (
	ReasonUnschedulable           = "node(s) were unschedulable"
	ReasonNodeAffinity            = "node(s) didn't match Pod's node affinity/selector"
	ReasonPodAffinity             = "node(s) didn't match pod affinity rules"
	ReasonPodAntiAffinity         = "node(s) didn't match pod anti-affinity rules"
	ReasonExistingPodAntiAffinity = "node(s) didn't satisfy existing pods anti-affinity rules"
	ReasonTooManyPods             = "Too many pods"
)

// NodeUnschedulable rejects cordoned nodes unless the Pod tolerates the
// node.kubernetes.io/unschedulable taint.
type NodeUnschedulable struct{}

func (NodeUnschedulable) Name() string { return "NodeUnschedulable" }

func (NodeUnschedulable) Filter(pod *corev1.Pod, node *framework.NodeInfo, _ *framework.Snapshot) []string {
	if !node.Node.Spec.Unschedulable {
		return nil
	}
	taint := corev1.Taint{Key: "node.kubernetes.io/unschedulable", Effect: corev1.TaintEffectNoSchedule}
	for _, t := range pod.Spec.Tolerations {
		if t.ToleratesTaint(taint) {
			return nil
		}
	}
	return []string{ReasonUnschedulable}
}

// TaintToleration rejects nodes with NoSchedule or NoExecute taints the Pod
// does not tolerate, and prefers nodes with fewer untolerated
// PreferNoSchedule taints.
type TaintToleration struct{}

func (TaintToleration) Name() string  { return "TaintToleration" }
func (TaintToleration) Weight() int64 { return 3 }

func (TaintToleration) Filter(pod *corev1.Pod, node *framework.NodeInfo, _ *framework.Snapshot) []string {
	if taint, ok := corev1.UntoleratedTaint(node.Node.Spec.Taints, pod.Spec.Tolerations); ok {
		return []string{"node(s) had untolerated taint " + taint.String()}
	}
	return nil
}

func (TaintToleration) Score(pod *corev1.Pod, nodes []*framework.NodeInfo, _ *framework.Snapshot) map[string]int64 {
	raw := map[string]int64{}
	for _, n := range nodes {
		raw[n.Node.Name] = 0
		for _, taint := range n.Node.Spec.Taints {
			if taint.Effect != corev1.TaintEffectPreferNoSchedule {
				continue
			}
			tolerated := false
			for _, t := range pod.Spec.Tolerations {
				if (t.Effect == "" || t.Effect == corev1.TaintEffectPreferNoSchedule) && t.ToleratesTaint(taint) {
					tolerated = true
				}
			}
			if !tolerated {
				raw[n.Node.Name]++
			}
		}
	}
	return framework.NormalizeMax(raw, true)
}

// NodeAffinity enforces nodeSelector and required node affinity, and adds
// the weights of the preferred terms a node matches.
type NodeAffinity struct{}

func (NodeAffinity) Name() string  { return "NodeAffinity" }
func (NodeAffinity) Weight() int64 { return 2 }

func (NodeAffinity) Filter(pod *corev1.Pod, node *framework.NodeInfo, _ *framework.Snapshot) []string {
	if !corev1.MatchesNodeSelector(&pod.Spec, node.Node) {
		return []string{ReasonNodeAffinity}
	}
	return nil
}

func (NodeAffinity) Score(pod *corev1.Pod, nodes []*framework.NodeInfo, _ *framework.Snapshot) map[string]int64 {
	raw := map[string]int64{}
	for _, n := range nodes {
		raw[n.Node.Name] = 0
		if a := pod.Spec.Affinity; a != nil && a.NodeAffinity != nil {
			for _, pref := range a.NodeAffinity.Preferred {
				if pref.Preference.Matches(n.Node) {
					raw[n.Node.Name] += int64(pref.Weight)
				}
			}
		}
	}
	return framework.NormalizeMax(raw, false)
}

// NodeResourcesFit rejects nodes whose allocatable resources cannot hold
// the Pod's requests, and prefers the least allocated nodes.
type NodeResourcesFit struct{}

func (NodeResourcesFit) Name() string  { return "NodeResourcesFit" }
func (NodeResourcesFit) Weight() int64 { return 1 }

func (NodeResourcesFit) Filter(pod *corev1.Pod, node *framework.NodeInfo, _ *framework.Snapshot) []string {
	want := framework.PodRequests(pod, false)
	alloc := framework.Allocatable(node.Node)
	var reasons []string
	if node.Requested.Pods+1 > alloc.Pods {
		reasons = append(reasons, ReasonTooManyPods)
	}
	if want.MilliCPU > 0 && node.Requested.MilliCPU+want.MilliCPU > alloc.MilliCPU {
		reasons = append(reasons, "Insufficient cpu")
	}
	if want.Memory > 0 && node.Requested.Memory+want.Memory > alloc.Memory {
		reasons = append(reasons, "Insufficient memory")
	}
	return reasons
}

func (NodeResourcesFit) Score(pod *corev1.Pod, nodes []*framework.NodeInfo, _ *framework.Snapshot) map[string]int64 {
	want := framework.PodRequests(pod, true)
	free := func(requested, capacity int64) int64 {
		if capacity == 0 || requested > capacity {
			return 0
		}
		return (capacity - requested) * framework.MaxScore / capacity
	}
	out := map[string]int64{}
	for _, n := range nodes {
		alloc := framework.Allocatable(n.Node)
		out[n.Node.Name] = (free(n.NonZeroRequested.MilliCPU+want.MilliCPU, alloc.MilliCPU) +
			free(n.NonZeroRequested.Memory+want.Memory, alloc.Memory)) / 2
	}
	return out
}

// PodTopologySpread is the default spreading: nodes running fewer Pods of
// the same controller score higher, so replicas spread across nodes.
type PodTopologySpread struct{}

func (PodTopologySpread) Name() string  { return "PodTopologySpread" }
func (PodTopologySpread) Weight() int64 { return 2 }

func (PodTopologySpread) Score(pod *corev1.Pod, nodes []*framework.NodeInfo, _ *framework.Snapshot) map[string]int64 {
	owner := controllerUID(pod)
	raw := map[string]int64{}
	for _, n := range nodes {
		raw[n.Node.Name] = 0
		if owner == "" {
			continue
		}
		for _, p := range n.Pods {
			if controllerUID(p) == owner && p.DeletionTimestamp == nil {
				raw[n.Node.Name]++
			}
		}
	}
	return framework.NormalizeMax(raw, true)
}

func controllerUID(p *corev1.Pod) string {
	for _, ref := range p.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return ref.UID
		}
	}
	return ""
}
