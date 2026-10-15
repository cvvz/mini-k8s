// Package job holds the API strategy for Jobs.
package job

import (
	batchv1 "mini-k8s/pkg/apis/batch/v1"
	"mini-k8s/pkg/apis/batch/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy defaults and validates Jobs. Unless the selector is manual, a
// new Job selects its Pods by its own UID and labels them (and itself)
// with its name, as Kubernetes does.
var Strategy = apiserver.Strategy{
	Defaults: func(obj metav1.Object) { batchv1.SetDefaultsJobSpec(&obj.(*batchv1.Job).Spec) },
	PrepareForCreate: func(obj metav1.Object) {
		j := obj.(*batchv1.Job)
		if j.Spec.ManualSelector != nil && *j.Spec.ManualSelector {
			return
		}
		j.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{batchv1.ControllerUIDLabel: j.UID}}
		tpl := &j.Spec.Template
		if tpl.Labels == nil {
			tpl.Labels = map[string]string{}
		}
		for k, v := range map[string]string{batchv1.ControllerUIDLabel: j.UID, batchv1.LegacyControllerUIDLabel: j.UID,
			batchv1.JobNameLabel: j.Name, batchv1.LegacyJobNameLabel: j.Name} {
			tpl.Labels[k] = v
		}
		if j.Labels == nil {
			j.Labels = map[string]string{}
		}
		for _, k := range []string{batchv1.JobNameLabel, batchv1.LegacyJobNameLabel} {
			if _, ok := j.Labels[k]; !ok {
				j.Labels[k] = j.Name
			}
		}
	},
	Validate: func(obj metav1.Object) []string { return validation.ValidateJob(obj.(*batchv1.Job)) },
	ValidateUpdate: func(obj, old metav1.Object) []string {
		return validation.ValidateJobUpdate(obj.(*batchv1.Job), old.(*batchv1.Job))
	},
}
