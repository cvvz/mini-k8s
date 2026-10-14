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
)

// ValidateDeployment validates a complete Deployment.
func ValidateDeployment(d *appsv1.Deployment) []string {
	var errs []string
	if *d.Spec.Replicas < 0 {
		errs = append(errs, "spec.replicas: Invalid value: must be greater than or equal to 0")
	}
	errs = append(errs, ValidateSelectorMatchesTemplate("spec", d.Spec.Selector, &d.Spec.Template)...)
	errs = append(errs, corevalidation.ValidatePodSpec("spec.template.spec", &d.Spec.Template.Spec)...)
	if p := d.Spec.Template.Spec.RestartPolicy; p != corev1.RestartPolicyAlways {
		errs = append(errs, fmt.Sprintf(`spec.template.spec.restartPolicy: Unsupported value: %q: supported values: "Always"`, p))
	}
	return errs
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
