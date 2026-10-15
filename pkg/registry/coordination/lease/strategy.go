// Package lease holds the API strategy for Leases.
package lease

import (
	"fmt"

	coordinationv1 "mini-k8s/pkg/apis/coordination/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy validates Leases.
var Strategy = apiserver.Strategy{
	Validate: func(obj metav1.Object) []string {
		if d := obj.(*coordinationv1.Lease).Spec.LeaseDurationSeconds; d != nil && *d <= 0 {
			return []string{fmt.Sprintf("spec.leaseDurationSeconds: Invalid value: %d: must be greater than 0", *d)}
		}
		return nil
	},
}
