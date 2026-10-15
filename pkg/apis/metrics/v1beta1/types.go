// Package v1beta1 contains the metrics.k8s.io/v1beta1 types (like
// k8s.io/metrics/pkg/apis/metrics/v1beta1): the CPU and memory usage of
// nodes and Pods that metrics-server serves and `kk top` and the
// HorizontalPodAutoscaler read. They are never stored.
package v1beta1

import (
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

// GroupVersion is the API group and version of the metrics API.
const GroupVersion = "metrics.k8s.io/v1beta1"

// NodeMetrics is the resource usage of a node.
type NodeMetrics struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	// Timestamp is when the usage was measured; Window the interval
	// it is averaged over.
	Timestamp metav1.Time         `json:"timestamp"`
	Window    metav1.Duration     `json:"window"`
	Usage     corev1.ResourceList `json:"usage"`
}

// NodeMetricsList is a list of NodeMetrics.
type NodeMetricsList struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        metav1.ListMeta `json:"metadata"`
	Items           []NodeMetrics   `json:"items"`
}

// PodMetrics is the resource usage of the containers of a Pod.
type PodMetrics struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Timestamp         metav1.Time        `json:"timestamp"`
	Window            metav1.Duration    `json:"window"`
	Containers        []ContainerMetrics `json:"containers"`
}

// PodMetricsList is a list of PodMetrics.
type PodMetricsList struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        metav1.ListMeta `json:"metadata"`
	Items           []PodMetrics    `json:"items"`
}

// ContainerMetrics is the resource usage of one container.
type ContainerMetrics struct {
	Name  string              `json:"name"`
	Usage corev1.ResourceList `json:"usage"`
}
