// Package scheme registers the served resources: their REST coordinates,
// kinds and constructors (the role of the Kubernetes scheme and RESTMapper).
package scheme

import (
	"net/url"
	"strings"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	batchv1 "mini-k8s/pkg/apis/batch/v1"
	coordinationv1 "mini-k8s/pkg/apis/coordination/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	networkingv1 "mini-k8s/pkg/apis/networking/v1"
	storagev1 "mini-k8s/pkg/apis/storage/v1"
)

// ResourceInfo describes one REST resource.
type ResourceInfo struct {
	Group      string // "" for the core group
	Version    string
	Resource   string // plural, lower case
	Singular   string
	Kind       string
	Namespaced bool
	ShortNames []string
	// HasStatus means the resource has a status subresource: the main
	// endpoint ignores status changes and /status ignores everything else.
	HasStatus bool
	New       func() metav1.Object
}

// GroupVersion returns "v1" or "group/v1".
func (r ResourceInfo) GroupVersion() string {
	if r.Group == "" {
		return r.Version
	}
	return r.Group + "/" + r.Version
}

// Prefix returns the REST prefix, "/api/v1" or "/apis/{group}/{version}".
func (r ResourceInfo) Prefix() string {
	if r.Group == "" {
		return "/api/" + r.Version
	}
	return "/apis/" + r.Group + "/" + r.Version
}

// Path builds a request path. An empty namespace on a namespaced resource
// addresses all namespaces (valid for list and watch only).
func (r ResourceInfo) Path(namespace, name string, subresource ...string) string {
	p := r.Prefix()
	if r.Namespaced && namespace != "" {
		p += "/namespaces/" + url.PathEscape(namespace)
	}
	p += "/" + r.Resource
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	for _, s := range subresource {
		p += "/" + s
	}
	return p
}

// GroupResource returns "deployments.apps" or "pods".
func (r ResourceInfo) GroupResource() string {
	if r.Group == "" {
		return r.Resource
	}
	return r.Resource + "." + r.Group
}

// Registered resources.
var (
	Namespaces = ResourceInfo{Version: "v1", Resource: "namespaces", Singular: "namespace", Kind: "Namespace",
		ShortNames: []string{"ns"}, HasStatus: true, New: func() metav1.Object { return &corev1.Namespace{} }}
	Nodes = ResourceInfo{Version: "v1", Resource: "nodes", Singular: "node", Kind: "Node",
		ShortNames: []string{"no"}, HasStatus: true, New: func() metav1.Object { return &corev1.Node{} }}
	Pods = ResourceInfo{Version: "v1", Resource: "pods", Singular: "pod", Kind: "Pod", Namespaced: true,
		ShortNames: []string{"po"}, HasStatus: true, New: func() metav1.Object { return &corev1.Pod{} }}
	Services = ResourceInfo{Version: "v1", Resource: "services", Singular: "service", Kind: "Service", Namespaced: true,
		ShortNames: []string{"svc"}, HasStatus: true, New: func() metav1.Object { return &corev1.Service{} }}
	Endpoints = ResourceInfo{Version: "v1", Resource: "endpoints", Singular: "endpoints", Kind: "Endpoints", Namespaced: true,
		ShortNames: []string{"ep"}, New: func() metav1.Object { return &corev1.Endpoints{} }}
	Events = ResourceInfo{Version: "v1", Resource: "events", Singular: "event", Kind: "Event", Namespaced: true,
		ShortNames: []string{"ev"}, New: func() metav1.Object { return &corev1.Event{} }}
	ConfigMaps = ResourceInfo{Version: "v1", Resource: "configmaps", Singular: "configmap", Kind: "ConfigMap", Namespaced: true,
		ShortNames: []string{"cm"}, New: func() metav1.Object { return &corev1.ConfigMap{} }}
	Secrets = ResourceInfo{Version: "v1", Resource: "secrets", Singular: "secret", Kind: "Secret", Namespaced: true,
		New: func() metav1.Object { return &corev1.Secret{} }}
	PersistentVolumes = ResourceInfo{Version: "v1", Resource: "persistentvolumes", Singular: "persistentvolume",
		Kind: "PersistentVolume", ShortNames: []string{"pv"}, HasStatus: true, New: func() metav1.Object { return &corev1.PersistentVolume{} }}
	PersistentVolumeClaims = ResourceInfo{Version: "v1", Resource: "persistentvolumeclaims", Singular: "persistentvolumeclaim",
		Kind: "PersistentVolumeClaim", Namespaced: true, ShortNames: []string{"pvc"}, HasStatus: true,
		New: func() metav1.Object { return &corev1.PersistentVolumeClaim{} }}
	StorageClasses = ResourceInfo{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses", Singular: "storageclass",
		Kind: "StorageClass", ShortNames: []string{"sc"}, New: func() metav1.Object { return &storagev1.StorageClass{} }}
	Deployments = ResourceInfo{Group: "apps", Version: "v1", Resource: "deployments", Singular: "deployment", Kind: "Deployment",
		Namespaced: true, ShortNames: []string{"deploy"}, HasStatus: true, New: func() metav1.Object { return &appsv1.Deployment{} }}
	ReplicaSets = ResourceInfo{Group: "apps", Version: "v1", Resource: "replicasets", Singular: "replicaset", Kind: "ReplicaSet",
		Namespaced: true, ShortNames: []string{"rs"}, HasStatus: true, New: func() metav1.Object { return &appsv1.ReplicaSet{} }}
	DaemonSets = ResourceInfo{Group: "apps", Version: "v1", Resource: "daemonsets", Singular: "daemonset", Kind: "DaemonSet",
		Namespaced: true, ShortNames: []string{"ds"}, HasStatus: true, New: func() metav1.Object { return &appsv1.DaemonSet{} }}
	ControllerRevisions = ResourceInfo{Group: "apps", Version: "v1", Resource: "controllerrevisions", Singular: "controllerrevision",
		Kind: "ControllerRevision", Namespaced: true, New: func() metav1.Object { return &appsv1.ControllerRevision{} }}
	Jobs = ResourceInfo{Group: "batch", Version: "v1", Resource: "jobs", Singular: "job", Kind: "Job", Namespaced: true,
		HasStatus: true, New: func() metav1.Object { return &batchv1.Job{} }}
	CronJobs = ResourceInfo{Group: "batch", Version: "v1", Resource: "cronjobs", Singular: "cronjob", Kind: "CronJob",
		Namespaced: true, ShortNames: []string{"cj"}, HasStatus: true, New: func() metav1.Object { return &batchv1.CronJob{} }}
	Leases = ResourceInfo{Group: "coordination.k8s.io", Version: "v1", Resource: "leases", Singular: "lease", Kind: "Lease",
		Namespaced: true, New: func() metav1.Object { return &coordinationv1.Lease{} }}
	Ingresses = ResourceInfo{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses", Singular: "ingress", Kind: "Ingress",
		Namespaced: true, ShortNames: []string{"ing"}, HasStatus: true, New: func() metav1.Object { return &networkingv1.Ingress{} }}
)

// All lists every registered resource in discovery order.
var All = []ResourceInfo{Namespaces, Nodes, Pods, Services, Endpoints, Events, ConfigMaps, Secrets, PersistentVolumes,
	PersistentVolumeClaims, Deployments, ReplicaSets, DaemonSets, ControllerRevisions, Jobs, CronJobs, Ingresses, Leases,
	StorageClasses}

// Lookup resolves a user-supplied name: plural, singular, short name, kind
// (case-insensitive) or "plural.group".
func Lookup(name string) (ResourceInfo, bool) {
	name = strings.ToLower(name)
	for _, r := range All {
		if name == r.Resource || name == r.Singular || name == strings.ToLower(r.Kind) || name == r.GroupResource() {
			return r, true
		}
		for _, s := range r.ShortNames {
			if name == s {
				return r, true
			}
		}
	}
	return ResourceInfo{}, false
}

// ForKind finds the resource for an apiVersion/kind pair; an empty
// apiVersion matches any group.
func ForKind(apiVersion, kind string) (ResourceInfo, bool) {
	for _, r := range All {
		if r.Kind == kind && (apiVersion == "" || apiVersion == r.GroupVersion()) {
			return r, true
		}
	}
	return ResourceInfo{}, false
}

// ForGroupVersionResource finds a resource by its REST coordinates.
func ForGroupVersionResource(group, version, resource string) (ResourceInfo, bool) {
	for _, r := range All {
		if r.Group == group && r.Version == version && r.Resource == resource {
			return r, true
		}
	}
	return ResourceInfo{}, false
}

// SetTypeMeta fills apiVersion and kind for obj from its registration.
func (r ResourceInfo) SetTypeMeta(obj metav1.Object) {
	t := obj.GetTypeMeta()
	t.APIVersion, t.Kind = r.GroupVersion(), r.Kind
}
