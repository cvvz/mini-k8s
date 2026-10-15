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

func service(mutate func(*corev1.ServiceSpec)) *corev1.ServiceSpec {
	spec := &corev1.ServiceSpec{Selector: map[string]string{"app": "web"}, Ports: []corev1.ServicePort{{Port: 80}}}
	if mutate != nil {
		mutate(spec)
	}
	corev1.SetDefaultsServiceSpec(spec)
	return spec
}

func TestServiceDefaults(t *testing.T) {
	spec := service(func(s *corev1.ServiceSpec) { s.SessionAffinity = corev1.ServiceAffinityClientIP })
	p := spec.Ports[0]
	if spec.Type != corev1.ServiceTypeClusterIP || p.Protocol != corev1.ProtocolTCP || p.TargetPort != intstr.FromInt(80) {
		t.Fatalf("defaults not applied: %+v", spec)
	}
	if *spec.SessionAffinityConfig.ClientIP.TimeoutSeconds != 10800 {
		t.Fatalf("ClientIP affinity timeout defaults to 3h, got %+v", spec.SessionAffinityConfig)
	}
	if errs := ValidateServiceSpec(spec); len(errs) != 0 {
		t.Fatalf("valid Service rejected: %v", errs)
	}
	if errs := ValidateServiceSpec(service(func(s *corev1.ServiceSpec) { s.Selector = nil })); len(errs) != 0 {
		t.Fatalf("Services without a selector are allowed: %v", errs)
	}
}

func TestValidateServiceSpec(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*corev1.ServiceSpec)
		want   string
	}{
		"unknown type":   {func(s *corev1.ServiceSpec) { s.Type = "LoadBalancer" }, `spec.type: Unsupported value: "LoadBalancer"`},
		"bad cluster IP": {func(s *corev1.ServiceSpec) { s.ClusterIP = "10.96.0" }, `spec.clusterIP: Invalid value: "10.96.0"`},
		"no ports":       {func(s *corev1.ServiceSpec) { s.Ports = nil }, "spec.ports: Required value"},
		"unnamed ports": {func(s *corev1.ServiceSpec) { s.Ports = []corev1.ServicePort{{Port: 80}, {Port: 81}} },
			"spec.ports[0].name: Required value"},
		"duplicate port": {func(s *corev1.ServiceSpec) {
			s.Ports = []corev1.ServicePort{{Name: "a", Port: 80}, {Name: "b", Port: 80}}
		}, `spec.ports[1]: Duplicate value: "80/TCP"`},
		"bad protocol": {func(s *corev1.ServiceSpec) { s.Ports[0].Protocol = "SCTP" },
			`spec.ports[0].protocol: Unsupported value: "SCTP": supported values: "TCP", "UDP"`},
		"bad target port name": {func(s *corev1.ServiceSpec) { s.Ports[0].TargetPort = intstr.FromString("Web_Port") },
			`spec.ports[0].targetPort: Invalid value: "Web_Port"`},
		"node port on ClusterIP": {func(s *corev1.ServiceSpec) { s.Ports[0].NodePort = 30080 },
			"spec.ports[0].nodePort: Forbidden: may not be used when `type` is 'ClusterIP'"},
		"affinity timeout": {func(s *corev1.ServiceSpec) {
			s.SessionAffinity = corev1.ServiceAffinityClientIP
			zero := int32(0)
			s.SessionAffinityConfig = &corev1.SessionAffinityConfig{ClientIP: &corev1.ClientIPConfig{TimeoutSeconds: &zero}}
		}, "spec.sessionAffinityConfig.clientIP.timeoutSeconds: Invalid value: 0"},
	} {
		t.Run(name, func(t *testing.T) {
			errs := ValidateServiceSpec(service(tc.mutate))
			if !strings.Contains(strings.Join(errs, "\n"), tc.want) {
				t.Fatalf("want %q in %v", tc.want, errs)
			}
		})
	}
	if errs := ValidateServiceSpec(service(func(s *corev1.ServiceSpec) {
		s.Type = corev1.ServiceTypeNodePort
		s.Ports = []corev1.ServicePort{{Name: "tcp", Port: 53, NodePort: 30053}, {Name: "udp", Port: 53, Protocol: corev1.ProtocolUDP, NodePort: 30053}}
	})); len(errs) != 0 {
		t.Fatalf("the same port and node port over TCP and UDP is valid: %v", errs)
	}
}

func TestValidateServiceUpdate(t *testing.T) {
	old := service(func(s *corev1.ServiceSpec) { s.ClusterIP = "10.96.0.20" })
	if errs := ValidateServiceUpdate(service(func(s *corev1.ServiceSpec) { s.ClusterIP = "10.96.0.20" }), old); len(errs) != 0 {
		t.Fatal(errs)
	}
	errs := ValidateServiceUpdate(service(func(s *corev1.ServiceSpec) { s.ClusterIP = "10.96.0.21" }), old)
	if len(errs) != 1 || errs[0] != `spec.clusterIP: Invalid value: "10.96.0.21": may not change once set` {
		t.Fatal(errs)
	}
}

func TestFindPort(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "a", Ports: []corev1.ContainerPort{{Name: "dns", ContainerPort: 5353, Protocol: corev1.ProtocolUDP}}},
		{Name: "b", Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}},
	}}}
	for _, tc := range []struct {
		port corev1.ServicePort
		want int32
	}{
		{corev1.ServicePort{Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(9090)}, 9090},
		{corev1.ServicePort{Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString("http")}, 8080},
		{corev1.ServicePort{Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromString("dns")}, 5353},
	} {
		if got, err := corev1.FindPort(pod, &tc.port); err != nil || got != tc.want {
			t.Fatalf("FindPort(%+v) = %d, %v; want %d", tc.port, got, err, tc.want)
		}
	}
	if _, err := corev1.FindPort(pod, &corev1.ServicePort{Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString("dns")}); err == nil {
		t.Fatal("a named port must match the protocol")
	}
}

func TestValidateEndpoints(t *testing.T) {
	valid := &corev1.Endpoints{Subsets: []corev1.EndpointSubset{{
		Addresses: []corev1.EndpointAddress{{IP: "10.244.1.5"}},
		Ports:     []corev1.EndpointPort{{Port: 8080, Protocol: corev1.ProtocolTCP}},
	}}}
	if errs := ValidateEndpoints(valid); len(errs) != 0 {
		t.Fatal(errs)
	}
	bad := &corev1.Endpoints{Subsets: []corev1.EndpointSubset{
		{NotReadyAddresses: []corev1.EndpointAddress{{IP: "nope"}}, Ports: []corev1.EndpointPort{{Port: 0, Protocol: "TCP"}}},
		{Ports: []corev1.EndpointPort{{Port: 1, Protocol: "TCP"}}},
	}}
	got := strings.Join(ValidateEndpoints(bad), "\n")
	for _, want := range []string{`subsets[0].notReadyAddresses[0].ip: Invalid value: "nope"`, "subsets[0].ports[0].port: Invalid value: 0",
		"subsets[1]: Required value: must specify `addresses` or `notReadyAddresses`"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in\n%s", want, got)
		}
	}
}

func TestHeadlessAndExternalNameServices(t *testing.T) {
	ok := []*corev1.ServiceSpec{
		service(func(s *corev1.ServiceSpec) { s.ClusterIP = corev1.ClusterIPNone; s.Ports = nil }),
		service(func(s *corev1.ServiceSpec) {
			s.Type, s.ExternalName, s.Ports, s.Selector = corev1.ServiceTypeExternalName, "db.example.com", nil, nil
		}),
	}
	for _, spec := range ok {
		if errs := ValidateServiceSpec(spec); len(errs) != 0 {
			t.Fatalf("%+v: %v", spec, errs)
		}
	}
	for want, mutate := range map[string]func(*corev1.ServiceSpec){
		"spec.externalName: Required value": func(s *corev1.ServiceSpec) { s.Type = corev1.ServiceTypeExternalName },
		`spec.clusterIP: Invalid value: "10.96.0.9": may not be set for ExternalName services`: func(s *corev1.ServiceSpec) {
			s.Type, s.ExternalName, s.ClusterIP = corev1.ServiceTypeExternalName, "a.example.com", "10.96.0.9"
		},
		`spec.clusterIP: Invalid value: "None": may not be set to 'None' for NodePort services`: func(s *corev1.ServiceSpec) {
			s.Type, s.ClusterIP = corev1.ServiceTypeNodePort, corev1.ClusterIPNone
		},
		"spec.externalName: Forbidden: may not be set when `type` is 'ClusterIP'": func(s *corev1.ServiceSpec) { s.ExternalName = "x.example.com" },
		`spec.externalName: Invalid value: "Not A Name"`: func(s *corev1.ServiceSpec) {
			s.Type, s.ExternalName = corev1.ServiceTypeExternalName, "Not A Name"
		},
	} {
		if errs := ValidateServiceSpec(service(mutate)); !strings.Contains(strings.Join(errs, "\n"), want) {
			t.Errorf("want %q in %v", want, errs)
		}
	}
	// Becoming an ExternalName drops the cluster IP; nothing else may change it.
	old := service(func(s *corev1.ServiceSpec) { s.ClusterIP = "10.96.0.20" })
	external := service(func(s *corev1.ServiceSpec) { s.Type, s.ExternalName = corev1.ServiceTypeExternalName, "a.example.com" })
	if errs := ValidateServiceUpdate(external, old); len(errs) != 0 {
		t.Fatal(errs)
	}
	if errs := ValidateServiceUpdate(service(func(s *corev1.ServiceSpec) { s.ClusterIP = corev1.ClusterIPNone }), old); len(errs) != 1 {
		t.Fatalf("a Service cannot become headless: %v", errs)
	}
}

func TestPodDNSAndHostname(t *testing.T) {
	spec := podSpec()
	if spec.DNSPolicy != corev1.DNSClusterFirst || !*spec.EnableServiceLinks {
		t.Fatalf("defaults: %q %v", spec.DNSPolicy, spec.EnableServiceLinks)
	}
	ndots := "2"
	spec.Hostname, spec.Subdomain = "db-0", "db"
	spec.DNSPolicy = corev1.DNSNone
	spec.DNSConfig = &corev1.PodDNSConfig{Nameservers: []string{"1.1.1.1"}, Searches: []string{"example.com"},
		Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: &ndots}}}
	if errs := ValidatePodSpec("spec", spec); len(errs) != 0 {
		t.Fatal(errs)
	}
	for name, tc := range map[string]struct {
		mutate func(*corev1.PodSpec)
		want   string
	}{
		"policy":        {func(s *corev1.PodSpec) { s.DNSPolicy = "Cluster" }, `spec.dnsPolicy: Unsupported value: "Cluster"`},
		"none needs ns": {func(s *corev1.PodSpec) { s.DNSPolicy = corev1.DNSNone }, "spec.dnsConfig: Required value"},
		"bad ns": {func(s *corev1.PodSpec) {
			s.DNSConfig = &corev1.PodDNSConfig{Nameservers: []string{"dns"}}
		}, `spec.dnsConfig.nameservers[0]: Invalid value: "dns"`},
		"too many ns": {func(s *corev1.PodSpec) {
			s.DNSConfig = &corev1.PodDNSConfig{Nameservers: []string{"1.1.1.1", "1.1.1.2", "1.1.1.3", "1.1.1.4"}}
		}, "must not have more than 3 nameservers"},
		"hostname": {func(s *corev1.PodSpec) { s.Hostname = "Db_0" }, `spec.hostname: Invalid value: "Db_0"`},
	} {
		s := podSpec()
		tc.mutate(s)
		if errs := ValidatePodSpec("spec", s); !strings.Contains(strings.Join(errs, "\n"), tc.want) {
			t.Errorf("%s: want %q in %v", name, tc.want, errs)
		}
	}
}
