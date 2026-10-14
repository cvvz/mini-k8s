// Package node holds the API strategy for Nodes.
package node

import (
	"net"
	"strconv"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy validates spec.podCIDR, which may be set once, and exposes
// spec.unschedulable as a selectable field.
var Strategy = apiserver.Strategy{
	Validate: func(obj metav1.Object) []string {
		if cidr := obj.(*corev1.Node).Spec.PodCIDR; cidr != "" {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return []string{"spec.podCIDR: Invalid value: " + err.Error()}
			}
		}
		return nil
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
