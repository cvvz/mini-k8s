package volume

import (
	"fmt"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/fieldpath"
)

// ObjectSource looks up the ConfigMaps and Secrets volumes refer to.
type ObjectSource interface {
	ConfigMap(namespace, name string) (*corev1.ConfigMap, bool)
	Secret(namespace, name string) (*corev1.Secret, bool)
}

// Source carries what payloads are built from.
type Source struct {
	Pod     *corev1.Pod
	Objects ObjectSource
	// Allocatable stands in for unset limits in resourceFieldRef values.
	Allocatable corev1.ResourceList
}

func modeOr(mode *int32, fallback int32) int32 {
	if mode != nil {
		return *mode
	}
	return fallback
}

func isTrue(b *bool) bool { return b != nil && *b }

// keyPayload projects the keys of a ConfigMap or Secret: every key, or the
// listed items (missing keys are an error unless optional).
func keyPayload(data map[string][]byte, items []corev1.KeyToPath, mode int32, optional bool, missingKey string) (map[string]FileProjection, error) {
	out := map[string]FileProjection{}
	if len(items) == 0 {
		for k, v := range data {
			out[k] = FileProjection{Data: v, Mode: mode}
		}
		return out, nil
	}
	for _, it := range items {
		v, ok := data[it.Key]
		if !ok {
			if optional {
				continue
			}
			return nil, fmt.Errorf("%s: %s", missingKey, it.Key)
		}
		out[it.Path] = FileProjection{Data: v, Mode: modeOr(it.Mode, mode)}
	}
	return out, nil
}

// configMapPayload projects a ConfigMap; a missing one is empty when
// optional.
func (s Source) configMapPayload(name string, items []corev1.KeyToPath, mode int32, optional bool) (map[string]FileProjection, error) {
	cm, ok := s.Objects.ConfigMap(s.Pod.Namespace, name)
	if !ok {
		if optional {
			return map[string]FileProjection{}, nil
		}
		return nil, fmt.Errorf("configmap %q not found", name)
	}
	data := map[string][]byte{}
	for k, v := range cm.Data {
		data[k] = []byte(v)
	}
	for k, v := range cm.BinaryData {
		data[k] = v
	}
	return keyPayload(data, items, mode, optional, "configmap references non-existent config key")
}

// secretPayload projects a Secret; a missing one is empty when optional.
func (s Source) secretPayload(name string, items []corev1.KeyToPath, mode int32, optional bool) (map[string]FileProjection, error) {
	secret, ok := s.Objects.Secret(s.Pod.Namespace, name)
	if !ok {
		if optional {
			return map[string]FileProjection{}, nil
		}
		return nil, fmt.Errorf("secret %q not found", name)
	}
	return keyPayload(secret.Data, items, mode, optional, "secret references non-existent secret key")
}

// downwardAPIPayload renders Pod fields (labels and annotations as sorted
// key="value" lines) and container resources.
func (s Source) downwardAPIPayload(items []corev1.DownwardAPIVolumeFile, mode int32) (map[string]FileProjection, error) {
	out := map[string]FileProjection{}
	for _, it := range items {
		var value string
		var err error
		switch {
		case it.FieldRef != nil:
			value, err = fieldpath.PodFieldValue(s.Pod, it.FieldRef.FieldPath, s.Pod.Status.PodIP, s.Pod.Status.HostIP)
		case it.ResourceFieldRef != nil:
			c := s.container(it.ResourceFieldRef.ContainerName)
			if c == nil {
				return nil, fmt.Errorf("container %q not found in Pod %s", it.ResourceFieldRef.ContainerName, s.Pod.Name)
			}
			value, err = fieldpath.ResourceValue(it.ResourceFieldRef, c, s.Allocatable)
		}
		if err != nil {
			return nil, err
		}
		out[it.Path] = FileProjection{Data: []byte(value), Mode: modeOr(it.Mode, mode)}
	}
	return out, nil
}

func (s Source) container(name string) *corev1.Container {
	for _, list := range [][]corev1.Container{s.Pod.Spec.Containers, s.Pod.Spec.InitContainers} {
		for i := range list {
			if list[i].Name == name {
				return &list[i]
			}
		}
	}
	return nil
}

// Payload returns the files of a configMap, secret, downwardAPI or projected
// volume; ok is false for other volume types. Errors are worded like the
// kubelet's MountVolume.SetUp failures.
func (s Source) Payload(v corev1.VolumeSource) (payload map[string]FileProjection, ok bool, err error) {
	switch {
	case v.ConfigMap != nil:
		c := v.ConfigMap
		payload, err = s.configMapPayload(c.Name, c.Items, modeOr(c.DefaultMode, corev1.ProjectedDefaultMode), isTrue(c.Optional))
	case v.Secret != nil:
		c := v.Secret
		payload, err = s.secretPayload(c.SecretName, c.Items, modeOr(c.DefaultMode, corev1.ProjectedDefaultMode), isTrue(c.Optional))
	case v.DownwardAPI != nil:
		payload, err = s.downwardAPIPayload(v.DownwardAPI.Items, modeOr(v.DownwardAPI.DefaultMode, corev1.ProjectedDefaultMode))
	case v.Projected != nil:
		payload, err = s.projectedPayload(v.Projected)
	default:
		return nil, false, nil
	}
	return payload, true, err
}

// projectedPayload merges the files of every source.
func (s Source) projectedPayload(p *corev1.ProjectedVolumeSource) (map[string]FileProjection, error) {
	mode := modeOr(p.DefaultMode, corev1.ProjectedDefaultMode)
	out := map[string]FileProjection{}
	for _, src := range p.Sources {
		var part map[string]FileProjection
		var err error
		switch {
		case src.ConfigMap != nil:
			part, err = s.configMapPayload(src.ConfigMap.Name, src.ConfigMap.Items, mode, isTrue(src.ConfigMap.Optional))
		case src.Secret != nil:
			part, err = s.secretPayload(src.Secret.Name, src.Secret.Items, mode, isTrue(src.Secret.Optional))
		case src.DownwardAPI != nil:
			part, err = s.downwardAPIPayload(src.DownwardAPI.Items, mode)
		}
		if err != nil {
			return nil, err
		}
		for path, f := range part {
			out[path] = f
		}
	}
	return out, nil
}

// OnTmpfs reports whether a volume type lives in memory: secrets, the
// downward API and projected volumes, as in Kubernetes.
func OnTmpfs(v corev1.VolumeSource) bool {
	return v.Secret != nil || v.DownwardAPI != nil || v.Projected != nil
}

// PluginDir is the kubelet's per-type volume directory name, such as
// kubernetes.io~configmap.
func PluginDir(v corev1.VolumeSource) string {
	switch {
	case v.ConfigMap != nil:
		return "kubernetes.io~configmap"
	case v.Secret != nil:
		return "kubernetes.io~secret"
	case v.DownwardAPI != nil:
		return "kubernetes.io~downward-api"
	case v.Projected != nil:
		return "kubernetes.io~projected"
	}
	return ""
}
