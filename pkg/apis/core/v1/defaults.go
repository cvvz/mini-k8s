package v1

import "mini-k8s/pkg/util/ptr"

// SetDefaultsPodSpec fills omitted Pod spec fields.
func SetDefaultsPodSpec(spec *PodSpec) {
	if spec.TerminationGracePeriodSeconds == nil {
		spec.TerminationGracePeriodSeconds = ptr.To[int64](30)
	}
	if spec.RestartPolicy == "" {
		spec.RestartPolicy = RestartPolicyAlways
	}
	for _, list := range [][]Container{spec.InitContainers, spec.Containers} {
		for i := range list {
			c := &list[i]
			for j := range c.Ports {
				if c.Ports[j].Protocol == "" {
					c.Ports[j].Protocol = "TCP"
				}
			}
			// Requests default to the limits.
			for name, limit := range c.Resources.Limits {
				if _, ok := c.Resources.Requests[name]; !ok {
					if c.Resources.Requests == nil {
						c.Resources.Requests = ResourceList{}
					}
					c.Resources.Requests[name] = limit
				}
			}
			for _, p := range []*Probe{c.LivenessProbe, c.ReadinessProbe, c.StartupProbe} {
				setDefaultsProbe(p)
			}
		}
	}
}

func setDefaultsProbe(p *Probe) {
	if p == nil {
		return
	}
	defaults := []struct {
		field *int32
		value int32
	}{{&p.TimeoutSeconds, 1}, {&p.PeriodSeconds, 10}, {&p.SuccessThreshold, 1}, {&p.FailureThreshold, 3}}
	for _, d := range defaults {
		if *d.field == 0 {
			*d.field = d.value
		}
	}
	if p.HTTPGet != nil {
		if p.HTTPGet.Path == "" {
			p.HTTPGet.Path = "/"
		}
		if p.HTTPGet.Scheme == "" {
			p.HTTPGet.Scheme = "HTTP"
		}
	}
}

// GetPodQOS classifies a Pod: Guaranteed when every container limits CPU and
// memory and requests equal limits, BestEffort when no container requests
// or limits anything, Burstable otherwise.
func GetPodQOS(spec *PodSpec) string {
	requests, limits := map[string]int64{}, map[string]int64{}
	guaranteed := true
	for _, list := range [][]Container{spec.InitContainers, spec.Containers} {
		for _, c := range list {
			for _, name := range []string{ResourceCPU, ResourceMemory} {
				if q, ok := c.Resources.Requests[name]; ok && !q.IsZero() {
					requests[name] += q.MilliValue()
				}
				if q, ok := c.Resources.Limits[name]; ok && !q.IsZero() {
					limits[name] += q.MilliValue()
				} else {
					guaranteed = false
				}
			}
		}
	}
	if len(requests) == 0 && len(limits) == 0 {
		return PodQOSBestEffort
	}
	if guaranteed {
		for name, l := range limits {
			if requests[name] != l {
				guaranteed = false
			}
		}
	}
	if guaranteed {
		return PodQOSGuaranteed
	}
	return PodQOSBurstable
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
