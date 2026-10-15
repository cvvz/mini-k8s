package plugins

import (
	"slices"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/scheduler/framework"
)

// InterPodAffinity enforces required pod affinity and anti-affinity (the
// incoming Pod's and that of Pods already running) and scores preferred
// terms, like kube-scheduler's InterPodAffinity plugin.
type InterPodAffinity struct{}

func (InterPodAffinity) Name() string  { return "InterPodAffinity" }
func (InterPodAffinity) Weight() int64 { return 2 }

// termMatches reports whether p is selected by a term owned by a Pod in
// ownerNamespace (an empty namespace list means that namespace; a nil
// selector selects nothing).
func termMatches(term corev1.PodAffinityTerm, ownerNamespace string, p *corev1.Pod) bool {
	if term.LabelSelector == nil || p.DeletionTimestamp != nil {
		return false
	}
	namespaces := term.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{ownerNamespace}
	}
	if !slices.Contains(namespaces, p.Namespace) {
		return false
	}
	sel, err := term.LabelSelector.AsSelector()
	return err == nil && sel.Matches(p.Labels)
}

// sameDomain reports whether two nodes share the topology key's value
// (both must have the label).
func sameDomain(a, b *corev1.Node, key string) bool {
	va, oka := a.Labels[key]
	vb, okb := b.Labels[key]
	return oka && okb && va == vb
}

func required(pod *corev1.Pod) (affinity, anti []corev1.PodAffinityTerm) {
	if a := pod.Spec.Affinity; a != nil {
		if a.PodAffinity != nil {
			affinity = a.PodAffinity.Required
		}
		if a.PodAntiAffinity != nil {
			anti = a.PodAntiAffinity.Required
		}
	}
	return affinity, anti
}

func (InterPodAffinity) Filter(pod *corev1.Pod, node *framework.NodeInfo, snapshot *framework.Snapshot) []string {
	affinity, anti := required(pod)
	// Running Pods whose required anti-affinity selects this Pod keep it
	// out of their domain.
	for _, other := range snapshot.Nodes {
		for _, p := range other.Pods {
			_, theirs := required(p)
			for _, term := range theirs {
				if termMatches(term, p.Namespace, pod) && sameDomain(other.Node, node.Node, term.TopologyKey) {
					return []string{ReasonExistingPodAntiAffinity}
				}
			}
		}
	}
	for _, term := range anti {
		for _, other := range snapshot.Nodes {
			if !sameDomain(other.Node, node.Node, term.TopologyKey) {
				continue
			}
			for _, p := range other.Pods {
				if termMatches(term, pod.Namespace, p) {
					return []string{ReasonPodAntiAffinity}
				}
			}
		}
	}
	for _, term := range affinity {
		matchedAnywhere, matchedHere := false, false
		for _, other := range snapshot.Nodes {
			for _, p := range other.Pods {
				if termMatches(term, pod.Namespace, p) {
					matchedAnywhere = true
					if sameDomain(other.Node, node.Node, term.TopologyKey) {
						matchedHere = true
					}
				}
			}
		}
		// The first Pod of a group that selects itself may go anywhere
		// (with the topology label), as in Kubernetes.
		_, hasKey := node.Node.Labels[term.TopologyKey]
		if !matchedHere && (matchedAnywhere || !termMatches(term, pod.Namespace, pod) || !hasKey) {
			return []string{ReasonPodAffinity}
		}
	}
	return nil
}

// Score adds the weight of each preferred affinity term for every matching
// Pod in the node's domain and subtracts it for anti-affinity terms.
func (InterPodAffinity) Score(pod *corev1.Pod, nodes []*framework.NodeInfo, snapshot *framework.Snapshot) map[string]int64 {
	a := pod.Spec.Affinity
	raw := map[string]int64{}
	for _, n := range nodes {
		raw[n.Node.Name] = 0
	}
	if a == nil || (a.PodAffinity == nil && a.PodAntiAffinity == nil) {
		return raw
	}
	add := func(terms []corev1.WeightedPodAffinityTerm, sign int64) {
		for _, w := range terms {
			for _, n := range nodes {
				for _, other := range snapshot.Nodes {
					if !sameDomain(other.Node, n.Node, w.PodAffinityTerm.TopologyKey) {
						continue
					}
					for _, p := range other.Pods {
						if termMatches(w.PodAffinityTerm, pod.Namespace, p) {
							raw[n.Node.Name] += sign * int64(w.Weight)
						}
					}
				}
			}
		}
	}
	if a.PodAffinity != nil {
		add(a.PodAffinity.Preferred, 1)
	}
	if a.PodAntiAffinity != nil {
		add(a.PodAntiAffinity.Preferred, -1)
	}
	// Min-max normalisation to [0, MaxScore].
	lo, hi := int64(0), int64(0)
	first := true
	for _, v := range raw {
		if first || v < lo {
			lo = v
		}
		if first || v > hi {
			hi = v
		}
		first = false
	}
	out := map[string]int64{}
	for k, v := range raw {
		if hi == lo {
			out[k] = 0
		} else {
			out[k] = framework.MaxScore * (v - lo) / (hi - lo)
		}
	}
	return out
}
