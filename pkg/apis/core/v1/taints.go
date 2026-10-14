package v1

import "fmt"

// Taint effects.
const (
	// TaintEffectNoSchedule keeps Pods that do not tolerate the taint off the node.
	TaintEffectNoSchedule = "NoSchedule"
	// TaintEffectPreferNoSchedule is a soft NoSchedule (ignored by the scheduler here).
	TaintEffectPreferNoSchedule = "PreferNoSchedule"
	// TaintEffectNoExecute also evicts running Pods (eviction arrives in M7).
	TaintEffectNoExecute = "NoExecute"
)

// Toleration operators.
const (
	TolerationOpEqual  = "Equal"
	TolerationOpExists = "Exists"
)

// Well-known node labels and taints.
const (
	LabelNodeRolePrefix       = "node-role.kubernetes.io/"
	LabelNodeRoleControlPlane = "node-role.kubernetes.io/control-plane"
	TaintNodeRoleControlPlane = "node-role.kubernetes.io/control-plane"
)

// Taint repels Pods that do not tolerate it from a node.
type Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

// String formats a taint like the scheduler's messages: "{key: value}".
func (t Taint) String() string { return fmt.Sprintf("{%s: %s}", t.Key, t.Value) }

// Toleration lets a Pod schedule onto (or stay on) nodes with matching taints.
type Toleration struct {
	// Key is the taint key; empty with operator Exists matches every taint.
	Key string `json:"key,omitempty"`
	// Operator is Equal (the default) or Exists.
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	// Effect restricts the toleration to one effect; empty matches all.
	Effect string `json:"effect,omitempty"`
	// TolerationSeconds bounds how long a NoExecute taint is tolerated.
	TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`
}

// ToleratesTaint reports whether the toleration matches the taint.
func (t Toleration) ToleratesTaint(taint Taint) bool {
	if t.Effect != "" && t.Effect != taint.Effect {
		return false
	}
	if t.Key != "" && t.Key != taint.Key {
		return false
	}
	switch t.Operator {
	case "", TolerationOpEqual:
		return t.Key == taint.Key && t.Value == taint.Value
	case TolerationOpExists:
		return true
	}
	return false
}

// UntoleratedTaint returns the first NoSchedule or NoExecute taint of the
// node that no toleration matches.
func UntoleratedTaint(taints []Taint, tolerations []Toleration) (Taint, bool) {
	for _, taint := range taints {
		if taint.Effect == TaintEffectPreferNoSchedule {
			continue
		}
		tolerated := false
		for _, t := range tolerations {
			if t.ToleratesTaint(taint) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return taint, true
		}
	}
	return Taint{}, false
}
