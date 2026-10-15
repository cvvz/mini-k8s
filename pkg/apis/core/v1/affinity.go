package v1

import (
	"slices"
	"strconv"

	metav1 "mini-k8s/pkg/apis/meta/v1"
)

// Affinity holds a Pod's scheduling constraints toward nodes and other Pods.
type Affinity struct {
	NodeAffinity    *NodeAffinity    `json:"nodeAffinity,omitempty"`
	PodAffinity     *PodAffinity     `json:"podAffinity,omitempty"`
	PodAntiAffinity *PodAntiAffinity `json:"podAntiAffinity,omitempty"`
}

// NodeAffinity restricts (required) or steers (preferred) the nodes a Pod
// may run on.
type NodeAffinity struct {
	Required  *NodeSelector             `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
	Preferred []PreferredSchedulingTerm `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

// NodeSelector matches a node when any of its terms does.
type NodeSelector struct {
	NodeSelectorTerms []NodeSelectorTerm `json:"nodeSelectorTerms"`
}

// NodeSelectorTerm matches a node when all its requirements do.
type NodeSelectorTerm struct {
	MatchExpressions []NodeSelectorRequirement `json:"matchExpressions,omitempty"`
	// MatchFields supports metadata.name.
	MatchFields []NodeSelectorRequirement `json:"matchFields,omitempty"`
}

// Node selector operators.
const (
	NodeSelectorOpIn           = "In"
	NodeSelectorOpNotIn        = "NotIn"
	NodeSelectorOpExists       = "Exists"
	NodeSelectorOpDoesNotExist = "DoesNotExist"
	NodeSelectorOpGt           = "Gt"
	NodeSelectorOpLt           = "Lt"
)

// NodeSelectorRequirement compares a node label (or field) with values.
type NodeSelectorRequirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values,omitempty"`
}

// PreferredSchedulingTerm adds Weight (1-100) to nodes matching Preference.
type PreferredSchedulingTerm struct {
	Weight     int32            `json:"weight"`
	Preference NodeSelectorTerm `json:"preference"`
}

// PodAffinity places a Pod in the same topology domain as matching Pods.
type PodAffinity struct {
	Required  []PodAffinityTerm         `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
	Preferred []WeightedPodAffinityTerm `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

// PodAntiAffinity keeps a Pod out of topology domains with matching Pods.
type PodAntiAffinity struct {
	Required  []PodAffinityTerm         `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
	Preferred []WeightedPodAffinityTerm `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

// PodAffinityTerm selects Pods (in Namespaces, by default the Pod's own)
// and a node label that defines the topology domain, such as
// kubernetes.io/hostname.
type PodAffinityTerm struct {
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`
	Namespaces    []string              `json:"namespaces,omitempty"`
	TopologyKey   string                `json:"topologyKey"`
}

// WeightedPodAffinityTerm adds Weight (1-100) for each matching Pod.
type WeightedPodAffinityTerm struct {
	Weight          int32           `json:"weight"`
	PodAffinityTerm PodAffinityTerm `json:"podAffinityTerm"`
}

// Matches reports whether the node satisfies every requirement of the term
// (an empty term matches nothing, as in Kubernetes).
func (t NodeSelectorTerm) Matches(node *Node) bool {
	if len(t.MatchExpressions) == 0 && len(t.MatchFields) == 0 {
		return false
	}
	for _, r := range t.MatchExpressions {
		value, ok := node.Labels[r.Key]
		if !r.matches(value, ok) {
			return false
		}
	}
	for _, r := range t.MatchFields {
		if r.Key != "metadata.name" || !r.matches(node.Name, true) {
			return false
		}
	}
	return true
}

func (r NodeSelectorRequirement) matches(value string, present bool) bool {
	switch r.Operator {
	case NodeSelectorOpIn:
		return present && slices.Contains(r.Values, value)
	case NodeSelectorOpNotIn:
		return !present || !slices.Contains(r.Values, value)
	case NodeSelectorOpExists:
		return present
	case NodeSelectorOpDoesNotExist:
		return !present
	case NodeSelectorOpGt, NodeSelectorOpLt:
		if !present || len(r.Values) != 1 {
			return false
		}
		have, err1 := strconv.ParseInt(value, 10, 64)
		want, err2 := strconv.ParseInt(r.Values[0], 10, 64)
		if err1 != nil || err2 != nil {
			return false
		}
		if r.Operator == NodeSelectorOpGt {
			return have > want
		}
		return have < want
	}
	return false
}

// MatchesNodeSelector reports whether the node passes the Pod's
// nodeSelector and required node affinity.
func MatchesNodeSelector(spec *PodSpec, node *Node) bool {
	for k, v := range spec.NodeSelector {
		if got, ok := node.Labels[k]; !ok || got != v { // the label must exist, even with an empty value
			return false
		}
	}
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil || spec.Affinity.NodeAffinity.Required == nil {
		return true
	}
	for _, term := range spec.Affinity.NodeAffinity.Required.NodeSelectorTerms {
		if term.Matches(node) {
			return true
		}
	}
	return false
}
