package v1

import "mini-k8s/pkg/util/ptr"

// SetDefaultsPodSpec fills omitted Pod spec fields.
func SetDefaultsPodSpec(spec *PodSpec) {
	if spec.TerminationGracePeriodSeconds == nil {
		spec.TerminationGracePeriodSeconds = ptr.To[int64](30)
	}
	for i := range spec.Containers {
		for j := range spec.Containers[i].Ports {
			if spec.Containers[i].Ports[j].Protocol == "" {
				spec.Containers[i].Ports[j].Protocol = "TCP"
			}
		}
	}
}

// SetDefaultsServiceSpec fills omitted Service port fields.
func SetDefaultsServiceSpec(spec *ServiceSpec) {
	for i := range spec.Ports {
		p := &spec.Ports[i]
		if p.Protocol == "" {
			p.Protocol = "TCP"
		}
		if p.TargetPort == 0 {
			p.TargetPort = p.Port
		}
	}
}
