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
	return validateRollingUpdate("spec.strategy.rollingUpdate", ru.MaxUnavailable, ru.MaxSurge)
}

// validateRollingUpdate checks maxUnavailable and maxSurge: non-negative
// numbers or percentages, not both zero.
func validateRollingUpdate(path string, maxUnavailable, maxSurge *intstr.IntOrString) []string {
	var errs []string
	values := map[string]int{}
	for _, f := range []struct {
		name string
		v    *intstr.IntOrString
	}{{"maxUnavailable", maxUnavailable}, {"maxSurge", maxSurge}} {
		n, err := intstr.GetScaledValue(f.v, 100, true)
		switch {
		case err != nil:
			errs = append(errs, fmt.Sprintf("%s.%s: Invalid value: %v", path, f.name, err))
		case n < 0:
			errs = append(errs, fmt.Sprintf("%s.%s: Invalid value: must be greater than or equal to 0", path, f.name))
		case f.name == "maxUnavailable" && f.v != nil && f.v.IsString && n > 100:
			errs = append(errs, path+".maxUnavailable: Invalid value: must not be greater than 100%")
		}
		values[f.name] = n
	}
	if len(errs) == 0 && values["maxUnavailable"] == 0 && values["maxSurge"] == 0 {
		errs = append(errs, path+".maxUnavailable: Invalid value: may not be 0 when `maxSurge` is 0")
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

// ValidateDaemonSet validates a complete DaemonSet.
func ValidateDaemonSet(ds *appsv1.DaemonSet) []string {
	errs := ValidateSelectorMatchesTemplate("spec", ds.Spec.Selector, &ds.Spec.Template)
	errs = append(errs, validatePodTemplate(&ds.Spec.Template)...)
	if ds.Spec.MinReadySeconds < 0 {
		errs = append(errs, "spec.minReadySeconds: Invalid value: must be greater than or equal to 0")
	}
	if l := ds.Spec.RevisionHistoryLimit; l != nil && *l < 0 {
		errs = append(errs, "spec.revisionHistoryLimit: Invalid value: must be greater than or equal to 0")
	}
	s := ds.Spec.UpdateStrategy
	switch s.Type {
	case appsv1.OnDeleteDaemonSetStrategyType:
	case appsv1.RollingUpdateDaemonSetStrategyType:
		if ru := s.RollingUpdate; ru != nil {
			errs = append(errs, validateRollingUpdate("spec.updateStrategy.rollingUpdate", ru.MaxUnavailable, ru.MaxSurge)...)
		}
	default:
		errs = append(errs, fmt.Sprintf(`spec.updateStrategy.type: Unsupported value: %q: supported values: "RollingUpdate", "OnDelete"`, s.Type))
	}
	return errs
}

// ValidateDaemonSetUpdate rejects changes to the selector.
func ValidateDaemonSetUpdate(ds, old *appsv1.DaemonSet) []string {
	if !reflect.DeepEqual(ds.Spec.Selector, old.Spec.Selector) {
		return []string{"spec.selector: Invalid value: field is immutable"}
	}
	return nil
}

// ValidateControllerRevision requires a non-negative revision; the data of
// a revision never changes.
func ValidateControllerRevision(cr *appsv1.ControllerRevision) []string {
	if cr.Revision < 0 {
		return []string{fmt.Sprintf("revision: Invalid value: %d: must be greater than or equal to 0", cr.Revision)}
	}
	return nil
}

// ValidateControllerRevisionUpdate keeps the data.
func ValidateControllerRevisionUpdate(cr, old *appsv1.ControllerRevision) []string {
	if string(cr.Data) != string(old.Data) {
		return []string{"data: Invalid value: field is immutable"}
	}
	return nil
}
