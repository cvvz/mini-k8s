// Package validation validates apps API objects.
package validation

import (
	"fmt"
	"reflect"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	corevalidation "mini-k8s/pkg/apis/core/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/labels"
	"mini-k8s/pkg/util/intstr"
)

// ValidateDeployment validates a complete Deployment.
func ValidateDeployment(d *appsv1.Deployment) []string {
	var errs []string
	if *d.Spec.Replicas < 0 {
		errs = append(errs, "spec.replicas: Invalid value: must be greater than or equal to 0")
	}
	errs = append(errs, ValidateSelectorMatchesTemplate("spec", d.Spec.Selector, &d.Spec.Template)...)
	errs = append(errs, validatePodTemplate(&d.Spec.Template)...)
	errs = append(errs, validateStrategy(d.Spec.Strategy)...)
	if d.Spec.MinReadySeconds < 0 {
		errs = append(errs, "spec.minReadySeconds: Invalid value: must be greater than or equal to 0")
	}
	if l := d.Spec.RevisionHistoryLimit; l != nil && *l < 0 {
		errs = append(errs, "spec.revisionHistoryLimit: Invalid value: must be greater than or equal to 0")
	}
	if p := d.Spec.ProgressDeadlineSeconds; p != nil && *p <= d.Spec.MinReadySeconds {
		errs = append(errs, "spec.progressDeadlineSeconds: Invalid value: must be greater than minReadySeconds")
	}
	return errs
}

// validatePodTemplate checks the Pod spec of a template; Pods of controllers
// must always be restarted.
func validatePodTemplate(tpl *corev1.PodTemplateSpec) []string {
	errs := corevalidation.ValidatePodSpec("spec.template.spec", &tpl.Spec)
	if p := tpl.Spec.RestartPolicy; p != corev1.RestartPolicyAlways {
		errs = append(errs, fmt.Sprintf(`spec.template.spec.restartPolicy: Unsupported value: %q: supported values: "Always"`, p))
	}
	return errs
}

func validateStrategy(s appsv1.DeploymentStrategy) []string {
	switch s.Type {
	case appsv1.RecreateDeploymentStrategyType:
		if s.RollingUpdate != nil {
			return []string{"spec.strategy.rollingUpdate: Forbidden: may not be specified when strategy `type` is 'Recreate'"}
		}
		return nil
	case appsv1.RollingUpdateDeploymentStrategyType:
	default:
		return []string{fmt.Sprintf(`spec.strategy.type: Unsupported value: %q: supported values: "Recreate", "RollingUpdate"`, s.Type)}
	}
	ru := s.RollingUpdate
	if ru == nil {
		return nil
	}
	var errs []string
	values := map[string]int{}
	for name, v := range map[string]*intstr.IntOrString{"maxUnavailable": ru.MaxUnavailable, "maxSurge": ru.MaxSurge} {
		n, err := intstr.GetScaledValue(v, 100, true)
		switch {
		case err != nil:
			errs = append(errs, fmt.Sprintf("spec.strategy.rollingUpdate.%s: Invalid value: %v", name, err))
		case n < 0:
			errs = append(errs, fmt.Sprintf("spec.strategy.rollingUpdate.%s: Invalid value: must be greater than or equal to 0", name))
		case name == "maxUnavailable" && v.IsString && n > 100:
			errs = append(errs, "spec.strategy.rollingUpdate.maxUnavailable: Invalid value: must not be greater than 100%")
		}
		values[name] = n
	}
	if len(errs) == 0 && values["maxUnavailable"] == 0 && values["maxSurge"] == 0 {
		errs = append(errs, "spec.strategy.rollingUpdate.maxUnavailable: Invalid value: may not be 0 when `maxSurge` is 0")
	}
	return errs
}

// ValidateReplicaSet validates a complete ReplicaSet.
func ValidateReplicaSet(rs *appsv1.ReplicaSet) []string {
	var errs []string
	if *rs.Spec.Replicas < 0 {
		errs = append(errs, "spec.replicas: Invalid value: must be greater than or equal to 0")
	}
	if rs.Spec.MinReadySeconds < 0 {
		errs = append(errs, "spec.minReadySeconds: Invalid value: must be greater than or equal to 0")
	}
	errs = append(errs, ValidateSelectorMatchesTemplate("spec", rs.Spec.Selector, &rs.Spec.Template)...)
	return append(errs, validatePodTemplate(&rs.Spec.Template)...)
}

// ValidateReplicaSetUpdate rejects changes to the selector.
func ValidateReplicaSetUpdate(rs, old *appsv1.ReplicaSet) []string {
	if !reflect.DeepEqual(rs.Spec.Selector, old.Spec.Selector) {
		return []string{"spec.selector: Invalid value: field is immutable"}
	}
	return nil
}

// ValidateDeploymentUpdate rejects changes to immutable fields.
func ValidateDeploymentUpdate(d, old *appsv1.Deployment) []string {
	if !reflect.DeepEqual(d.Spec.Selector, old.Spec.Selector) {
		return []string{"spec.selector: Invalid value: field is immutable"}
	}
	return nil
}

// ValidateSelectorMatchesTemplate requires a non-empty selector that selects
// the Pods built from the template.
func ValidateSelectorMatchesTemplate(path string, ls *metav1.LabelSelector, tpl *corev1.PodTemplateSpec) []string {
	if ls == nil || (len(ls.MatchLabels) == 0 && len(ls.MatchExpressions) == 0) {
		return []string{path + ".selector: Required value: a non-empty selector is required"}
	}
	sel, err := ls.AsSelector()
	if err != nil {
		return []string{path + ".selector: Invalid value: " + err.Error()}
	}
	if err := labels.ValidateLabels(tpl.Labels); err != nil {
		return []string{path + ".template.metadata.labels: Invalid value: " + err.Error()}
	}
	if !sel.Matches(tpl.Labels) {
		return []string{path + ".template.metadata.labels: Invalid value: selector does not match template labels"}
	}
	return nil
}
