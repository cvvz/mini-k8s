package v1

import (
	"testing"

	"mini-k8s/pkg/api/resource"
)

func TestGetPodQOS(t *testing.T) {
	q := resource.MustParse
	both := ResourceList{ResourceCPU: q("500m"), ResourceMemory: q("64Mi")}
	for name, tc := range map[string]struct {
		containers []Container
		want       string
	}{
		"no resources":                 {[]Container{{Name: "a"}}, PodQOSBestEffort},
		"limits only":                  {[]Container{{Name: "a", Resources: ResourceRequirements{Limits: both}}}, PodQOSGuaranteed},
		"requests below limit":         {[]Container{{Name: "a", Resources: ResourceRequirements{Limits: both, Requests: ResourceList{ResourceCPU: q("100m")}}}}, PodQOSBurstable},
		"one container without limits": {[]Container{{Name: "a", Resources: ResourceRequirements{Limits: both}}, {Name: "b"}}, PodQOSBurstable},
		"memory request only":          {[]Container{{Name: "a", Resources: ResourceRequirements{Requests: ResourceList{ResourceMemory: q("1Gi")}}}}, PodQOSBurstable},
	} {
		spec := &PodSpec{Containers: tc.containers}
		SetDefaultsPodSpec(spec)
		if got := GetPodQOS(spec); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}
