// Package v1 contains the coordination.k8s.io/v1 types (coordinationv1):
// Lease, which kubelets renew in kube-node-lease as their heartbeat.
package v1

import (
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

// Lease records who holds something and until when.
type Lease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              LeaseSpec `json:"spec"`
}

// LeaseSpec is the holder and the timing of a Lease.
type LeaseSpec struct {
	HolderIdentity       *string           `json:"holderIdentity,omitempty"`
	LeaseDurationSeconds *int32            `json:"leaseDurationSeconds,omitempty"`
	AcquireTime          *metav1.MicroTime `json:"acquireTime,omitempty"`
	RenewTime            *metav1.MicroTime `json:"renewTime,omitempty"`
	LeaseTransitions     *int32            `json:"leaseTransitions,omitempty"`
}
