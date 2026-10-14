// Package node holds the API strategy for Nodes.
package node

import (
	"strconv"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy exposes spec.unschedulable as a selectable field.
var Strategy = apiserver.Strategy{
	Fields: func(obj metav1.Object) map[string]string {
		return map[string]string{"spec.unschedulable": strconv.FormatBool(obj.(*corev1.Node).Spec.Unschedulable)}
	},
}
