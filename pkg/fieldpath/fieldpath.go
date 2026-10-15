// Package fieldpath resolves the downward API: Pod fields named by a field
// path such as metadata.labels['app'], and container requests and limits,
// like k8s.io/kubernetes/pkg/fieldpath and pkg/api/v1/resource.
package fieldpath

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"mini-k8s/pkg/api/resource"
	corev1 "mini-k8s/pkg/apis/core/v1"
)

// podFieldLabels are the Pod fields a field path may name (the field labels
// of the v1 Pod conversion).
var podFieldLabels = map[string]bool{
	"metadata.annotations": true, "metadata.labels": true, "metadata.name": true, "metadata.namespace": true,
	"metadata.uid": true, "spec.nodeName": true, "spec.restartPolicy": true, "spec.schedulerName": true,
	"spec.serviceAccountName": true, "spec.hostNetwork": true, "status.phase": true, "status.podIP": true,
	"status.podIPs": true, "status.nominatedNodeName": true, "status.hostIP": true, "status.hostIPs": true,
}

// IsPodField reports whether path (without a subscript) names a Pod field.
func IsPodField(path string) bool { return podFieldLabels[path] }

// SplitMaybeSubscriptedPath splits "metadata.labels['app']" into
// "metadata.labels" and "app"; ok is false without a subscript.
func SplitMaybeSubscriptedPath(fieldPath string) (path, subscript string, ok bool) {
	if !strings.HasSuffix(fieldPath, "']") {
		return fieldPath, "", false
	}
	s := strings.TrimSuffix(fieldPath, "']")
	parts := strings.SplitN(s, "['", 2)
	if len(parts) < 2 || len(parts[0]) == 0 {
		return fieldPath, "", false
	}
	return parts[0], parts[1], true
}

// FormatMap renders labels or annotations as sorted key="value" lines, the
// format of downward API files.
func FormatMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, len(keys))
	for i, k := range keys {
		lines[i] = fmt.Sprintf("%s=%q", k, m[k])
	}
	return strings.Join(lines, "\n")
}

// PodFieldValue returns the value of a Pod field. The IPs are passed in
// because a container may start before the Pod's status reports them.
func PodFieldValue(pod *corev1.Pod, fieldPath, podIP, hostIP string) (string, error) {
	if path, subscript, ok := SplitMaybeSubscriptedPath(fieldPath); ok {
		switch path {
		case "metadata.labels":
			return pod.Labels[subscript], nil
		case "metadata.annotations":
			return pod.Annotations[subscript], nil
		}
		return "", fmt.Errorf("fieldPath %q does not support subscript", fieldPath)
	}
	switch fieldPath {
	case "metadata.name":
		return pod.Name, nil
	case "metadata.namespace":
		return pod.Namespace, nil
	case "metadata.uid":
		return pod.UID, nil
	case "metadata.labels":
		return FormatMap(pod.Labels), nil
	case "metadata.annotations":
		return FormatMap(pod.Annotations), nil
	case "spec.nodeName":
		return pod.Spec.NodeName, nil
	case "spec.serviceAccountName":
		return "", nil // mini-k8s has no service accounts
	case "status.podIP", "status.podIPs":
		return podIP, nil
	case "status.hostIP", "status.hostIPs":
		return hostIP, nil
	}
	return "", fmt.Errorf("unsupported fieldPath: %v", fieldPath)
}

// ResourceValue returns a container's request or limit scaled by the
// selector's divisor and rounded up, like the kubelet: a missing limit
// counts as the node's allocatable amount.
func ResourceValue(fs *corev1.ResourceFieldSelector, c *corev1.Container, allocatable corev1.ResourceList) (string, error) {
	divisor := resource.MustParse("1")
	if fs.Divisor != nil && !fs.Divisor.IsZero() {
		divisor = *fs.Divisor
	}
	kind, name, ok := strings.Cut(fs.Resource, ".")
	if !ok || (kind != "limits" && kind != "requests") {
		return "", fmt.Errorf("unsupported container resource: %v", fs.Resource)
	}
	var q resource.Quantity
	if kind == "limits" {
		q, ok = c.Resources.Limits[name]
		if !ok {
			q = allocatable[name]
		}
	} else {
		q = c.Resources.Requests[name]
	}
	var v float64
	switch name {
	case corev1.ResourceCPU:
		v = math.Ceil(float64(q.MilliValue()) / float64(divisor.MilliValue()))
	case corev1.ResourceMemory, "ephemeral-storage":
		v = math.Ceil(float64(q.Value()) / float64(divisor.Value()))
	default:
		return "", fmt.Errorf("unsupported container resource: %v", fs.Resource)
	}
	return strconv.FormatInt(int64(v), 10), nil
}
