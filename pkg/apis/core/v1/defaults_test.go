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

func TestImagePullPolicyDefaults(t *testing.T) {
	for image, want := range map[string]string{
		"nginx": PullAlways, "nginx:latest": PullAlways, "nginx:1.27": PullIfNotPresent, "mini-k8s:local": PullIfNotPresent,
		"registry:5000/team/app": PullAlways, "registry:5000/team/app:v2": PullIfNotPresent, "app@sha256:abc": PullIfNotPresent,
	} {
		spec := &PodSpec{Containers: []Container{{Name: "c", Image: image}}}
		SetDefaultsPodSpec(spec)
		if got := spec.Containers[0].ImagePullPolicy; got != want {
			t.Errorf("%s: %s, want %s", image, got, want)
		}
	}
}
