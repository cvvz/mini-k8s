package kuberuntime

import (
	"fmt"
	"sort"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/fieldpath"
	"mini-k8s/pkg/util/expansion"
)

// envList keeps variables in the order they were first defined.
type envList struct {
	names  []string
	values map[string]string
}

func (l *envList) set(name, value string) {
	if _, ok := l.values[name]; !ok {
		l.names = append(l.names, name)
	}
	l.values[name] = value
}

// containerEnv computes a container's environment like the kubelet: envFrom
// first, then env (overriding it), with $(VAR) references in values expanded
// from the variables defined so far and the Service variables, which are
// added last unless the container defines the same names. A missing
// ConfigMap, Secret or key that is not optional is an error.
func (m *Manager) containerEnv(pod *corev1.Pod, c corev1.Container, serviceEnv []corev1.EnvVar, podIP string) ([]corev1.EnvVar, error) {
	env := &envList{values: map[string]string{}}
	configMap := func(name string, optional *bool) (*corev1.ConfigMap, bool, error) {
		if m.Objects != nil {
			if cm, ok := m.Objects.ConfigMap(pod.Namespace, name); ok {
				return cm, true, nil
			}
		}
		if optional != nil && *optional {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("configmap %q not found", name)
	}
	secret := func(name string, optional *bool) (*corev1.Secret, bool, error) {
		if m.Objects != nil {
			if s, ok := m.Objects.Secret(pod.Namespace, name); ok {
				return s, true, nil
			}
		}
		if optional != nil && *optional {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("secret %q not found", name)
	}
	for _, from := range c.EnvFrom {
		switch {
		case from.ConfigMapRef != nil:
			cm, ok, err := configMap(from.ConfigMapRef.Name, from.ConfigMapRef.Optional)
			if err != nil {
				return nil, err
			}
			if ok {
				for _, k := range sortedKeys(cm.Data) {
					env.set(from.Prefix+k, cm.Data[k])
				}
			}
		case from.SecretRef != nil:
			s, ok, err := secret(from.SecretRef.Name, from.SecretRef.Optional)
			if err != nil {
				return nil, err
			}
			if ok {
				for _, k := range sortedKeys(s.Data) {
					env.set(from.Prefix+k, string(s.Data[k]))
				}
			}
		}
	}
	services := map[string]string{}
	for _, e := range serviceEnv {
		services[e.Name] = e.Value
	}
	mapping := expansion.MappingFuncFor(env.values, services)
	for _, e := range c.Env {
		value := e.Value
		if value != "" {
			value = expansion.Expand(value, mapping)
		} else if from := e.ValueFrom; from != nil {
			var err error
			switch {
			case from.FieldRef != nil:
				value, err = fieldpath.PodFieldValue(pod, from.FieldRef.FieldPath, podIP, m.NodeIP)
			case from.ResourceFieldRef != nil:
				value, err = fieldpath.ResourceValue(from.ResourceFieldRef, &c, m.Allocatable)
			case from.ConfigMapKeyRef != nil:
				ref := from.ConfigMapKeyRef
				cm, ok, cerr := configMap(ref.Name, ref.Optional)
				if cerr != nil {
					return nil, cerr
				}
				if !ok {
					continue
				}
				if value, ok = cm.Data[ref.Key]; !ok {
					if ref.Optional != nil && *ref.Optional {
						continue
					}
					return nil, fmt.Errorf("couldn't find key %s in ConfigMap %s/%s", ref.Key, pod.Namespace, ref.Name)
				}
			case from.SecretKeyRef != nil:
				ref := from.SecretKeyRef
				s, ok, serr := secret(ref.Name, ref.Optional)
				if serr != nil {
					return nil, serr
				}
				if !ok {
					continue
				}
				data, found := s.Data[ref.Key]
				if !found {
					if ref.Optional != nil && *ref.Optional {
						continue
					}
					return nil, fmt.Errorf("couldn't find key %s in Secret %s/%s", ref.Key, pod.Namespace, ref.Name)
				}
				value = string(data)
			}
			if err != nil {
				return nil, err
			}
		}
		env.set(e.Name, value)
	}
	out := make([]corev1.EnvVar, 0, len(env.names)+len(serviceEnv))
	for _, e := range serviceEnv {
		if _, own := env.values[e.Name]; !own {
			out = append(out, e)
		}
	}
	for _, name := range env.names {
		out = append(out, corev1.EnvVar{Name: name, Value: env.values[name]})
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// expandCommand expands $(VAR) references in a container's command and
// arguments from its final environment.
func expandCommand(c corev1.Container, env []corev1.EnvVar) (command, args []string) {
	vars := map[string]string{}
	for _, e := range env {
		vars[e.Name] = e.Value
	}
	mapping := expansion.MappingFuncFor(vars)
	expand := func(list []string) []string {
		if list == nil {
			return nil
		}
		out := make([]string, len(list))
		for i, s := range list {
			out[i] = expansion.Expand(s, mapping)
		}
		return out
	}
	return expand(c.Command), expand(c.Args)
}
