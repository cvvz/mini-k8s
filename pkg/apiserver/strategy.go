package apiserver

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"

	metav1 "mini-k8s/pkg/apis/meta/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	appsv1 "mini-k8s/pkg/apis/apps/v1"
	"mini-k8s/pkg/labels"
	"mini-k8s/pkg/scheme"
	"mini-k8s/pkg/util/ptr"
)

// strategy holds the resource-specific parts of create/update handling.
type strategy struct {
	// defaults fills omitted fields; it runs on create and on update.
	defaults func(obj metav1.Object)
	// prepareForCreate initialises server-owned state after status is reset.
	prepareForCreate func(obj metav1.Object)
	// validate returns "field: message" errors for a complete object.
	validate func(obj metav1.Object) []string
	// validateUpdate returns errors for disallowed changes.
	validateUpdate func(obj, old metav1.Object) []string
	// nameIsLabel requires names to be DNS labels (63 chars, no dots).
	nameIsLabel bool
}

var strategies = map[string]strategy{
	scheme.Namespaces.Resource: {
		nameIsLabel: true,
		prepareForCreate: func(obj metav1.Object) {
			ns := obj.(*corev1.Namespace)
			ns.Status.Phase = corev1.NamespaceActive
			if !ns.HasFinalizer(corev1.FinalizerKubernetes) {
				ns.Finalizers = append(ns.Finalizers, corev1.FinalizerKubernetes)
			}
		},
	},
	scheme.Nodes.Resource: {},
	scheme.Pods.Resource: {
		defaults:         func(obj metav1.Object) { defaultPodSpec(&obj.(*corev1.Pod).Spec) },
		prepareForCreate: func(obj metav1.Object) { obj.(*corev1.Pod).Status.Phase = corev1.PodPending },
		validate:         func(obj metav1.Object) []string { return validatePodSpec("spec", &obj.(*corev1.Pod).Spec) },
		validateUpdate: func(obj, old metav1.Object) []string {
			if !reflect.DeepEqual(obj.(*corev1.Pod).Spec, old.(*corev1.Pod).Spec) {
				return []string{"spec: Forbidden: pod spec is immutable after creation"}
			}
			return nil
		},
	},
	scheme.Services.Resource: {
		nameIsLabel: true,
		defaults: func(obj metav1.Object) {
			for i := range obj.(*corev1.Service).Spec.Ports {
				p := &obj.(*corev1.Service).Spec.Ports[i]
				if p.Protocol == "" {
					p.Protocol = "TCP"
				}
				if p.TargetPort == 0 {
					p.TargetPort = p.Port
				}
			}
		},
		validate: func(obj metav1.Object) []string { return validateServiceSpec(&obj.(*corev1.Service).Spec) },
	},
	scheme.Events.Resource: {
		defaults: func(obj metav1.Object) {
			e := obj.(*corev1.Event)
			if e.Count == 0 {
				e.Count = 1
			}
			if e.Type == "" {
				e.Type = corev1.EventTypeNormal
			}
			if e.FirstTimestamp.IsZero() {
				e.FirstTimestamp = metav1.Now()
			}
			if e.LastTimestamp.IsZero() {
				e.LastTimestamp = e.FirstTimestamp
			}
		},
		validate: func(obj metav1.Object) []string {
			e := obj.(*corev1.Event)
			var errs []string
			if e.InvolvedObject.Name == "" || e.InvolvedObject.Kind == "" {
				errs = append(errs, "involvedObject: Required value: kind and name are required")
			}
			if e.Type != corev1.EventTypeNormal && e.Type != corev1.EventTypeWarning {
				errs = append(errs, "type: Unsupported value: must be Normal or Warning")
			}
			return errs
		},
	},
	scheme.Deployments.Resource: {
		defaults: func(obj metav1.Object) {
			d := obj.(*appsv1.Deployment)
			if d.Spec.Replicas == nil {
				d.Spec.Replicas = ptr.To[int32](1)
			}
			defaultPodSpec(&d.Spec.Template.Spec)
		},
		validate: func(obj metav1.Object) []string {
			d := obj.(*appsv1.Deployment)
			var errs []string
			if *d.Spec.Replicas < 0 {
				errs = append(errs, "spec.replicas: Invalid value: must be greater than or equal to 0")
			}
			errs = append(errs, validateSelectorMatchesTemplate("spec", d.Spec.Selector, &d.Spec.Template)...)
			return append(errs, validatePodSpec("spec.template.spec", &d.Spec.Template.Spec)...)
		},
		validateUpdate: func(obj, old metav1.Object) []string {
			if !reflect.DeepEqual(obj.(*appsv1.Deployment).Spec.Selector, old.(*appsv1.Deployment).Spec.Selector) {
				return []string{"spec.selector: Invalid value: field is immutable"}
			}
			return nil
		},
	},
}

var (
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	envName      = regexp.MustCompile(`^[-._a-zA-Z][-._a-zA-Z0-9]*$`)
)

func validName(name string, label bool) bool {
	if label {
		return dnsLabel.MatchString(name)
	}
	return len(name) <= 253 && dnsSubdomain.MatchString(name)
}

func defaultPodSpec(spec *corev1.PodSpec) {
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

func validatePodSpec(path string, spec *corev1.PodSpec) []string {
	var errs []string
	if len(spec.Containers) != 1 {
		errs = append(errs, path+".containers: Invalid value: exactly one container is supported")
	}
	for i, c := range spec.Containers {
		p := fmt.Sprintf("%s.containers[%d]", path, i)
		if !dnsLabel.MatchString(c.Name) {
			errs = append(errs, p+".name: Invalid value: must be a DNS label")
		}
		if c.Image == "" {
			errs = append(errs, p+".image: Required value")
		}
		for _, e := range c.Env {
			if !envName.MatchString(e.Name) {
				errs = append(errs, fmt.Sprintf("%s.env: Invalid value: %q is not a valid variable name", p, e.Name))
			}
		}
		for _, port := range c.Ports {
			if port.ContainerPort < 1 || port.ContainerPort > 65535 {
				errs = append(errs, p+".ports: Invalid value: containerPort must be 1-65535")
			}
			if port.Protocol != "TCP" {
				errs = append(errs, p+".ports: Unsupported value: only TCP is supported")
			}
		}
	}
	if g := spec.TerminationGracePeriodSeconds; g != nil && *g < 0 {
		errs = append(errs, path+".terminationGracePeriodSeconds: Invalid value: must be >= 0")
	}
	if spec.NodeName != "" && !validName(spec.NodeName, false) {
		errs = append(errs, path+".nodeName: Invalid value: must be a DNS subdomain")
	}
	return errs
}

func validateSelectorMatchesTemplate(path string, ls *metav1.LabelSelector, tpl *corev1.PodTemplateSpec) []string {
	if ls == nil || (len(ls.MatchLabels) == 0 && len(ls.MatchExpressions) == 0) {
		return []string{path + ".selector: Required value: a non-empty selector is required"}
	}
	sel, err := ls.AsSelector()
	if err != nil {
		return []string{path + ".selector: Invalid value: " + err.Error()}
	}
	if err := labels.ValidateLabels(tpl.Labels); err != nil {
		return []string{path + ".template.metadata.labels: Invalid value: " + err.Error()}
	}
	if !sel.Matches(tpl.Labels) {
		return []string{path + ".template.metadata.labels: Invalid value: selector does not match template labels"}
	}
	return nil
}

func validateServiceSpec(spec *corev1.ServiceSpec) []string {
	var errs []string
	if len(spec.Selector) == 0 {
		errs = append(errs, "spec.selector: Required value")
	}
	if err := labels.ValidateLabels(spec.Selector); err != nil {
		errs = append(errs, "spec.selector: Invalid value: "+err.Error())
	}
	// Until ClusterIP networking lands, a Service is the single HTTP gateway
	// published on 127.0.0.1:18080.
	if len(spec.Ports) != 1 || spec.Ports[0].Port != 80 {
		errs = append(errs, "spec.ports: Invalid value: exactly one port 80 is supported (the gateway is 127.0.0.1:18080)")
	}
	for _, p := range spec.Ports {
		if p.TargetPort < 1 || p.TargetPort > 65535 {
			errs = append(errs, "spec.ports.targetPort: Invalid value: must be 1-65535")
		}
		if p.Protocol != "TCP" {
			errs = append(errs, "spec.ports.protocol: Unsupported value: only TCP is supported")
		}
	}
	return errs
}

// fieldsOf returns the selectable fields of an object.
func fieldsOf(obj metav1.Object) map[string]string {
	m := obj.GetObjectMeta()
	f := map[string]string{"metadata.name": m.Name, "metadata.namespace": m.Namespace}
	switch o := obj.(type) {
	case *corev1.Pod:
		f["spec.nodeName"] = o.Spec.NodeName
		f["status.phase"] = o.Status.Phase
	case *corev1.Node:
		f["spec.unschedulable"] = strconv.FormatBool(o.Spec.Unschedulable)
	case *corev1.Namespace:
		f["status.phase"] = o.Status.Phase
	case *corev1.Event:
		f["involvedObject.kind"] = o.InvolvedObject.Kind
		f["involvedObject.name"] = o.InvolvedObject.Name
		f["involvedObject.namespace"] = o.InvolvedObject.Namespace
		f["involvedObject.uid"] = o.InvolvedObject.UID
		f["reason"] = o.Reason
		f["type"] = o.Type
	}
	return f
}
