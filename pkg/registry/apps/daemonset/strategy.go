// Package daemonset holds the API strategy for DaemonSets.
package daemonset

import (
	"reflect"
	"strconv"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	"mini-k8s/pkg/apis/apps/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy defaults and validates DaemonSets, keeps the selector and counts
// template changes in the deprecated.daemonset.template.generation
// annotation, which the controller copies onto Pods.
var Strategy = apiserver.Strategy{
	Defaults: func(obj metav1.Object) { appsv1.SetDefaultsDaemonSet(obj.(*appsv1.DaemonSet)) },
	PrepareForCreate: func(obj metav1.Object) {
		setTemplateGeneration(obj.(*appsv1.DaemonSet), 1)
	},
	PrepareForUpdate: func(obj, old metav1.Object) {
		ds, prev := obj.(*appsv1.DaemonSet), old.(*appsv1.DaemonSet)
		generation, _ := strconv.ParseInt(prev.Annotations[appsv1.DeprecatedTemplateGeneration], 10, 64)
		if !reflect.DeepEqual(ds.Spec.Template, prev.Spec.Template) {
			generation++
		}
		setTemplateGeneration(ds, max(generation, 1))
	},
	Validate: func(obj metav1.Object) []string { return validation.ValidateDaemonSet(obj.(*appsv1.DaemonSet)) },
	ValidateUpdate: func(obj, old metav1.Object) []string {
		return validation.ValidateDaemonSetUpdate(obj.(*appsv1.DaemonSet), old.(*appsv1.DaemonSet))
	},
}

func setTemplateGeneration(ds *appsv1.DaemonSet, generation int64) {
	if ds.Annotations == nil {
		ds.Annotations = map[string]string{}
	}
	ds.Annotations[appsv1.DeprecatedTemplateGeneration] = strconv.FormatInt(generation, 10)
}
