// Package node holds the API strategy for Nodes.
package node

import (
	"net"
	"strconv"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/apis/core/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy validates taints and spec.podCIDR, which may be set once, and exposes
// spec.unschedulable as a selectable field.
var Strategy = apiserver.Strategy{
	Validate: func(obj metav1.Object) []string {
		node := obj.(*corev1.Node)
		errs := validation.ValidateTaints("spec.taints", node.Spec.Taints)
		if cidr := node.Spec.PodCIDR; cidr != "" {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				errs = append(errs, "spec.podCIDR: Invalid value: "+err.Error())
			}
		}
		return errs
	},
	ValidateUpdate: func(obj, old metav1.Object) []string {
		prev, cur := old.(*corev1.Node).Spec.PodCIDR, obj.(*corev1.Node).Spec.PodCIDR
		if prev != "" && cur != prev {
			return []string{"spec.podCIDR: Forbidden: podCIDR cannot be changed once set"}
		}
		return nil
	},
	Fields: func(obj metav1.Object) map[string]string {
		return map[string]string{"spec.unschedulable": strconv.FormatBool(obj.(*corev1.Node).Spec.Unschedulable)}
	},
}
