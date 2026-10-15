package v1

import (
	"strings"

	"mini-k8s/pkg/util/intstr"
	"mini-k8s/pkg/util/ptr"
)

// SetDefaultsPodSpec fills omitted Pod spec fields.
func SetDefaultsPodSpec(spec *PodSpec) {
	if spec.TerminationGracePeriodSeconds == nil {
		spec.TerminationGracePeriodSeconds = ptr.To[int64](30)
	}
	if spec.RestartPolicy == "" {
		spec.RestartPolicy = RestartPolicyAlways
	}
	if spec.DNSPolicy == "" {
		spec.DNSPolicy = DNSClusterFirst
	}
	if spec.EnableServiceLinks == nil {
		spec.EnableServiceLinks = ptr.To(true)
	}
	for _, list := range [][]Container{spec.InitContainers, spec.Containers} {
		for i := range list {
			c := &list[i]
			if c.ImagePullPolicy == "" {
				// As in Kubernetes: always pull ":latest" and untagged images.
				c.ImagePullPolicy = PullIfNotPresent
				if tag := imageTag(c.Image); tag == "" || tag == "latest" {
					c.ImagePullPolicy = PullAlways
				}
			}
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
			for j := range c.Env {
				if from := c.Env[j].ValueFrom; from != nil {
					setDefaultsFieldRef(from.FieldRef)
				}
			}
		}
	}
	for i := range spec.Volumes {
		setDefaultsVolume(&spec.Volumes[i].VolumeSource)
	}
}

func setDefaultsFieldRef(f *ObjectFieldSelector) {
	if f != nil && f.APIVersion == "" {
		f.APIVersion = "v1"
	}
}

// setDefaultsVolume gives projected files the default mode 0644 and field
// references the v1 API version, as Kubernetes does.
func setDefaultsVolume(v *VolumeSource) {
	mode := func(m **int32) {
		if *m == nil {
			*m = ptr.To(ProjectedDefaultMode)
		}
	}
	downward := func(items []DownwardAPIVolumeFile) {
		for i := range items {
			setDefaultsFieldRef(items[i].FieldRef)
		}
	}
	switch {
	case v.ConfigMap != nil:
		mode(&v.ConfigMap.DefaultMode)
	case v.Secret != nil:
		mode(&v.Secret.DefaultMode)
	case v.DownwardAPI != nil:
		mode(&v.DownwardAPI.DefaultMode)
		downward(v.DownwardAPI.Items)
	case v.Projected != nil:
		mode(&v.Projected.DefaultMode)
		for _, s := range v.Projected.Sources {
			if s.DownwardAPI != nil {
				downward(s.DownwardAPI.Items)
			}
		}
	}
}

// imageTag returns the tag of an image reference ("" without one; digests
// count as tags).
func imageTag(image string) string {
	if i := strings.LastIndexByte(image, '@'); i >= 0 {
		return image[i+1:]
	}
	name := image[strings.LastIndexByte(image, '/')+1:]
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		return name[i+1:]
	}
	return ""
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

// SetDefaultsServiceSpec fills omitted Service fields as Kubernetes does.
func SetDefaultsServiceSpec(spec *ServiceSpec) {
	if spec.Type == "" {
		spec.Type = ServiceTypeClusterIP
	}
	if spec.SessionAffinity == "" {
		spec.SessionAffinity = ServiceAffinityNone
	}
	if spec.SessionAffinity == ServiceAffinityClientIP {
		if spec.SessionAffinityConfig == nil {
			spec.SessionAffinityConfig = &SessionAffinityConfig{}
		}
		if spec.SessionAffinityConfig.ClientIP == nil {
			spec.SessionAffinityConfig.ClientIP = &ClientIPConfig{}
		}
		if spec.SessionAffinityConfig.ClientIP.TimeoutSeconds == nil {
			t := DefaultClientIPServiceAffinitySeconds
			spec.SessionAffinityConfig.ClientIP.TimeoutSeconds = &t
		}
	}
	for i := range spec.Ports {
		p := &spec.Ports[i]
		if p.Protocol == "" {
			p.Protocol = ProtocolTCP
		}
		if !p.TargetPort.IsString && p.TargetPort.IntVal == 0 {
			p.TargetPort = intstr.FromInt(int(p.Port))
		}
	}
}

// SetDefaultsEndpoints fills omitted port protocols.
func SetDefaultsEndpoints(e *Endpoints) {
	for i := range e.Subsets {
		for j := range e.Subsets[i].Ports {
			if e.Subsets[i].Ports[j].Protocol == "" {
				e.Subsets[i].Ports[j].Protocol = ProtocolTCP
			}
		}
	}
}
