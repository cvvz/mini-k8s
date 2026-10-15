package validation

import (
	"fmt"
	"strconv"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/labels"
)

// validateScheduling checks nodeSelector and affinity like Kubernetes.
func validateScheduling(path string, spec *corev1.PodSpec) []string {
	var errs []string
	if err := labels.ValidateLabels(spec.NodeSelector); err != nil {
		errs = append(errs, path+".nodeSelector: Invalid value: "+err.Error())
	}
	a := spec.Affinity
	if a == nil {
		return errs
	}
	if na := a.NodeAffinity; na != nil {
		if na.Required != nil {
			p := path + ".affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution"
			if len(na.Required.NodeSelectorTerms) == 0 {
				errs = append(errs, p+".nodeSelectorTerms: Required value: must have at least one node selector term")
			}
			for i, t := range na.Required.NodeSelectorTerms {
				errs = append(errs, validateNodeSelectorTerm(fmt.Sprintf("%s.nodeSelectorTerms[%d]", p, i), t)...)
			}
		}
		for i, pref := range na.Preferred {
			p := fmt.Sprintf("%s.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[%d]", path, i)
			errs = append(errs, validateWeight(p, pref.Weight)...)
			errs = append(errs, validateNodeSelectorTerm(p+".preference", pref.Preference)...)
		}
	}
	for _, group := range []struct {
		name      string
		required  []corev1.PodAffinityTerm
		preferred []corev1.WeightedPodAffinityTerm
	}{
		{"podAffinity", podAffinityRequired(a), podAffinityPreferred(a)},
		{"podAntiAffinity", podAntiAffinityRequired(a), podAntiAffinityPreferred(a)},
	} {
		for i, t := range group.required {
			errs = append(errs, validatePodAffinityTerm(fmt.Sprintf("%s.affinity.%s.requiredDuringSchedulingIgnoredDuringExecution[%d]", path, group.name, i), t)...)
		}
		for i, w := range group.preferred {
			p := fmt.Sprintf("%s.affinity.%s.preferredDuringSchedulingIgnoredDuringExecution[%d]", path, group.name, i)
			errs = append(errs, validateWeight(p, w.Weight)...)
			errs = append(errs, validatePodAffinityTerm(p+".podAffinityTerm", w.PodAffinityTerm)...)
		}
	}
	return errs
}

func podAffinityRequired(a *corev1.Affinity) []corev1.PodAffinityTerm {
	if a.PodAffinity == nil {
		return nil
	}
	return a.PodAffinity.Required
}

func podAffinityPreferred(a *corev1.Affinity) []corev1.WeightedPodAffinityTerm {
	if a.PodAffinity == nil {
		return nil
	}
	return a.PodAffinity.Preferred
}

func podAntiAffinityRequired(a *corev1.Affinity) []corev1.PodAffinityTerm {
	if a.PodAntiAffinity == nil {
		return nil
	}
	return a.PodAntiAffinity.Required
}

func podAntiAffinityPreferred(a *corev1.Affinity) []corev1.WeightedPodAffinityTerm {
	if a.PodAntiAffinity == nil {
		return nil
	}
	return a.PodAntiAffinity.Preferred
}

func validateWeight(path string, w int32) []string {
	if w < 1 || w > 100 {
		return []string{fmt.Sprintf("%s.weight: Invalid value: %d: must be in the range 1-100", path, w)}
	}
	return nil
}

func validateNodeSelectorTerm(path string, t corev1.NodeSelectorTerm) []string {
	var errs []string
	for i, r := range t.MatchExpressions {
		p := fmt.Sprintf("%s.matchExpressions[%d]", path, i)
		if r.Key == "" {
			errs = append(errs, p+".key: Required value")
		}
		switch r.Operator {
		case corev1.NodeSelectorOpIn, corev1.NodeSelectorOpNotIn:
			if len(r.Values) == 0 {
				errs = append(errs, p+".values: Required value: must be specified when `operator` is 'In' or 'NotIn'")
			}
		case corev1.NodeSelectorOpExists, corev1.NodeSelectorOpDoesNotExist:
			if len(r.Values) > 0 {
				errs = append(errs, p+".values: Forbidden: may not be specified when `operator` is 'Exists' or 'DoesNotExist'")
			}
		case corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
			if len(r.Values) != 1 {
				errs = append(errs, p+".values: Required value: must be specified single value when `operator` is 'Lt' or 'Gt'")
			} else if _, err := strconv.ParseInt(r.Values[0], 10, 64); err != nil {
				errs = append(errs, fmt.Sprintf("%s.values[0]: Invalid value: %q: must be an integer", p, r.Values[0]))
			}
		default:
			errs = append(errs, fmt.Sprintf(`%s.operator: Unsupported value: %q: supported values: "DoesNotExist", "Exists", "Gt", "In", "Lt", "NotIn"`, p, r.Operator))
		}
	}
	for i, r := range t.MatchFields {
		p := fmt.Sprintf("%s.matchFields[%d]", path, i)
		if r.Key != "metadata.name" {
			errs = append(errs, fmt.Sprintf(`%s.key: Unsupported value: %q: supported values: "metadata.name"`, p, r.Key))
		}
		if r.Operator != corev1.NodeSelectorOpIn && r.Operator != corev1.NodeSelectorOpNotIn {
			errs = append(errs, fmt.Sprintf(`%s.operator: Unsupported value: %q: supported values: "In", "NotIn"`, p, r.Operator))
		}
		if len(r.Values) != 1 {
			errs = append(errs, p+".values: Required value: must be only one value when `operator` is 'In' or 'NotIn' for node field selector")
		}
	}
	return errs
}

func validatePodAffinityTerm(path string, t corev1.PodAffinityTerm) []string {
	var errs []string
	if t.TopologyKey == "" {
		errs = append(errs, path+".topologyKey: Required value: can not be empty")
	}
	if t.LabelSelector != nil {
		if _, err := t.LabelSelector.AsSelector(); err != nil {
			errs = append(errs, path+".labelSelector: Invalid value: "+err.Error())
		}
	}
	return errs
}
