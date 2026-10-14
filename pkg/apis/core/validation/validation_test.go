package validation

import (
	"strings"
	"testing"

	"mini-k8s/pkg/api/resource"
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/util/intstr"
)

func podSpec() *corev1.PodSpec {
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "setup", Image: "img"}},
		Containers:     []corev1.Container{{Name: "web", Image: "img"}, {Name: "sidecar", Image: "img"}},
	}
	corev1.SetDefaultsPodSpec(spec)
	return spec
}

func TestValidatePodSpecAcceptsInitAndMultipleContainers(t *testing.T) {
	spec := podSpec()
	if spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Fatalf("restartPolicy defaults to Always, got %q", spec.RestartPolicy)
	}
	if errs := ValidatePodSpec("spec", spec); len(errs) != 0 {
		t.Fatalf("valid spec rejected: %v", errs)
	}
	for _, policy := range []string{corev1.RestartPolicyOnFailure, corev1.RestartPolicyNever} {
		spec.RestartPolicy = policy
		if errs := ValidatePodSpec("spec", spec); len(errs) != 0 {
			t.Fatalf("%s rejected: %v", policy, errs)
		}
	}
}

func TestValidatePodSpecRejectsInvalidSpecs(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*corev1.PodSpec)
		want   string
	}{
		"no containers":         {func(s *corev1.PodSpec) { s.Containers = nil }, "spec.containers: Required value"},
		"duplicate name":        {func(s *corev1.PodSpec) { s.Containers[1].Name = "web" }, `spec.containers[1].name: Duplicate value: "web"`},
		"name shared with init": {func(s *corev1.PodSpec) { s.InitContainers[0].Name = "web" }, `spec.containers[0].name: Duplicate value: "web"`},
		"init without image":    {func(s *corev1.PodSpec) { s.InitContainers[0].Image = "" }, "spec.initContainers[0].image: Required value"},
		"bad restart policy":    {func(s *corev1.PodSpec) { s.RestartPolicy = "Sometimes" }, `spec.restartPolicy: Unsupported value: "Sometimes"`},
	} {
		spec := podSpec()
		tc.mutate(spec)
		errs := strings.Join(ValidatePodSpec("spec", spec), "\n")
		if !strings.Contains(errs, tc.want) {
			t.Errorf("%s: want %q in %q", name, tc.want, errs)
		}
	}
}

func TestValidateProbesAndResources(t *testing.T) {
	probe := func() *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(8080)}}}
	}
	for name, tc := range map[string]struct {
		mutate func(*corev1.PodSpec)
		want   string
	}{
		"probe on init container": {func(s *corev1.PodSpec) { s.InitContainers[0].ReadinessProbe = probe() }, "spec.initContainers[0].readinessProbe: Forbidden"},
		"two handlers": {func(s *corev1.PodSpec) {
			p := probe()
			p.TCPSocket = &corev1.TCPSocketAction{Port: intstr.FromInt(80)}
			s.Containers[0].LivenessProbe = p
		}, "spec.containers[0].livenessProbe: Required value: must specify exactly one of exec, httpGet, tcpSocket"},
		"bad port": {func(s *corev1.PodSpec) {
			p := probe()
			p.HTTPGet.Port = intstr.FromInt(70000)
			s.Containers[0].ReadinessProbe = p
		}, "spec.containers[0].readinessProbe.httpGet.port: Invalid value: 70000"},
		"liveness success threshold": {func(s *corev1.PodSpec) {
			p := probe()
			p.SuccessThreshold = 2
			s.Containers[0].LivenessProbe = p
		}, "spec.containers[0].livenessProbe.successThreshold: Invalid value: 2: must be 1"},
		"unknown resource": {func(s *corev1.PodSpec) {
			s.Containers[0].Resources.Limits = corev1.ResourceList{"gpu": resource.MustParse("1")}
		}, `spec.containers[0].resources.limits[gpu]: Unsupported value: "gpu"`},
		"request above limit": {func(s *corev1.PodSpec) {
			s.Containers[0].Resources = corev1.ResourceRequirements{
				Limits: corev1.ResourceList{"cpu": resource.MustParse("1")}, Requests: corev1.ResourceList{"cpu": resource.MustParse("2")}}
		}, `spec.containers[0].resources.requests: Invalid value: "2": must be less than or equal to cpu limit of 1`},
	} {
		spec := podSpec()
		tc.mutate(spec)
		corev1.SetDefaultsPodSpec(spec)
		errs := strings.Join(ValidatePodSpec("spec", spec), "\n")
		if !strings.Contains(errs, tc.want) {
			t.Errorf("%s: want %q in %q", name, tc.want, errs)
		}
	}
	spec := podSpec()
	spec.Containers[0].ReadinessProbe = probe()
	spec.Containers[0].Resources.Limits = corev1.ResourceList{"cpu": resource.MustParse("500m"), "memory": resource.MustParse("64Mi")}
	corev1.SetDefaultsPodSpec(spec)
	if errs := ValidatePodSpec("spec", spec); len(errs) != 0 {
		t.Fatalf("valid spec rejected: %v", errs)
	}
	p := spec.Containers[0].ReadinessProbe
	if p.PeriodSeconds != 10 || p.TimeoutSeconds != 1 || p.FailureThreshold != 3 || p.SuccessThreshold != 1 || p.HTTPGet.Path != "/" {
		t.Fatalf("probe defaults: %+v", p)
	}
	if q := spec.Containers[0].Resources.Requests["cpu"]; q.String() != "500m" {
		t.Fatalf("requests default to limits: %+v", spec.Containers[0].Resources)
	}
}

func TestValidateVolumesAndMounts(t *testing.T) {
	dirType := "Directory"
	badType := "Socket"
	for name, tc := range map[string]struct {
		mutate func(*corev1.PodSpec)
		want   string
	}{
		"no source":      {func(s *corev1.PodSpec) { s.Volumes[0].EmptyDir = nil }, "spec.volumes[0]: Required value: must specify exactly one of emptyDir, hostPath"},
		"two sources":    {func(s *corev1.PodSpec) { s.Volumes[0].HostPath = &corev1.HostPathVolumeSource{Path: "/x"} }, "must specify exactly one of emptyDir, hostPath"},
		"duplicate name": {func(s *corev1.PodSpec) { s.Volumes = append(s.Volumes, s.Volumes[0]) }, `spec.volumes[1].name: Duplicate value: "data"`},
		"bad medium":     {func(s *corev1.PodSpec) { s.Volumes[0].EmptyDir.Medium = "Disk" }, `spec.volumes[0].emptyDir.medium: Unsupported value: "Disk"`},
		"relative host path": {func(s *corev1.PodSpec) {
			s.Volumes[0] = corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "var/x", Type: &dirType}}}
		}, `spec.volumes[0].hostPath.path: Invalid value: "var/x": must be an absolute path`},
		"bad host path type": {func(s *corev1.PodSpec) {
			s.Volumes[0] = corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/x", Type: &badType}}}
		}, `spec.volumes[0].hostPath.type: Unsupported value: "Socket"`},
		"unknown volume": {func(s *corev1.PodSpec) { s.Containers[0].VolumeMounts[0].Name = "other" }, `spec.containers[0].volumeMounts[0].name: Not found: "other"`},
		"relative mount": {func(s *corev1.PodSpec) { s.Containers[0].VolumeMounts[0].MountPath = "data" }, `spec.containers[0].volumeMounts[0].mountPath: Invalid value: "data": must be an absolute path`},
		"duplicate mount path": {func(s *corev1.PodSpec) {
			s.Containers[0].VolumeMounts = append(s.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "data", MountPath: "/data/"})
		}, `spec.containers[0].volumeMounts[1].mountPath: Invalid value: "/data/": must be unique`},
	} {
		spec := podSpec()
		spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
		if name == "no source" || name == "two sources" {
			if errs := ValidatePodSpec("spec", spec); len(errs) != 0 {
				t.Fatalf("valid volumes rejected: %v", errs)
			}
		}
		tc.mutate(spec)
		errs := strings.Join(ValidatePodSpec("spec", spec), "\n")
		if !strings.Contains(errs, tc.want) {
			t.Errorf("%s: want %q in %q", name, tc.want, errs)
		}
	}
}
