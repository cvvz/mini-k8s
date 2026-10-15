package kuberuntime

import (
	"testing"

	"mini-k8s/pkg/api/resource"
	corev1 "mini-k8s/pkg/apis/core/v1"
)

func TestLinuxResourcesMapRequestsAndLimitsToCgroups(t *testing.T) {
	q := resource.MustParse
	for name, tc := range map[string]struct {
		r                          corev1.ResourceRequirements
		shares, period, quota, mem int64
	}{
		"best effort": {corev1.ResourceRequirements{}, 2, 0, 0, 0},
		"limits": {corev1.ResourceRequirements{
			Limits:   corev1.ResourceList{"cpu": q("250m"), "memory": q("64Mi")},
			Requests: corev1.ResourceList{"cpu": q("250m"), "memory": q("64Mi")}}, 256, 100000, 25000, 64 << 20},
		"tiny cpu": {corev1.ResourceRequirements{
			Limits: corev1.ResourceList{"cpu": q("1m")}, Requests: corev1.ResourceList{"cpu": q("1m")}}, 2, 100000, 1000, 0},
		"two cpus requested": {corev1.ResourceRequirements{Requests: corev1.ResourceList{"cpu": q("2")}}, 2048, 0, 0, 0},
		"huge cpu": {corev1.ResourceRequirements{
			Limits: corev1.ResourceList{"cpu": q("300M")}, Requests: corev1.ResourceList{"cpu": q("300M")}}, 262144, 100000, 100_000_000_000, 0},
	} {
		got := linuxResources(tc.r)
		if got.CpuShares != tc.shares || got.CpuPeriod != tc.period || got.CpuQuota != tc.quota ||
			got.MemoryLimitInBytes != tc.mem || got.MemorySwapLimitInBytes != tc.mem {
			t.Errorf("%s: %+v", name, got)
		}
	}
	pod := testPod()
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{"memory": q("32Mi")}
	if cfg := containerConfig(pod, pod.Spec.Containers[0], 0, mountedVolumes{}, containerExtras{}); cfg.Linux.Resources.MemoryLimitInBytes != 32<<20 {
		t.Fatalf("container config: %+v", cfg.Linux.Resources)
	}
}
