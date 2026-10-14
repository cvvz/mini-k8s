package api

import (
	"net/url"
	"strings"
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
	New       func() Object
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
		ShortNames: []string{"ns"}, HasStatus: true, New: func() Object { return &Namespace{} }}
	Nodes = ResourceInfo{Version: "v1", Resource: "nodes", Singular: "node", Kind: "Node",
		ShortNames: []string{"no"}, HasStatus: true, New: func() Object { return &Node{} }}
	Pods = ResourceInfo{Version: "v1", Resource: "pods", Singular: "pod", Kind: "Pod", Namespaced: true,
		ShortNames: []string{"po"}, HasStatus: true, New: func() Object { return &Pod{} }}
	Services = ResourceInfo{Version: "v1", Resource: "services", Singular: "service", Kind: "Service", Namespaced: true,
		ShortNames: []string{"svc"}, HasStatus: true, New: func() Object { return &Service{} }}
	Events = ResourceInfo{Version: "v1", Resource: "events", Singular: "event", Kind: "Event", Namespaced: true,
		ShortNames: []string{"ev"}, New: func() Object { return &Event{} }}
	Deployments = ResourceInfo{Group: "apps", Version: "v1", Resource: "deployments", Singular: "deployment", Kind: "Deployment",
		Namespaced: true, ShortNames: []string{"deploy"}, HasStatus: true, New: func() Object { return &Deployment{} }}
)

// All lists every registered resource in discovery order.
var All = []ResourceInfo{Namespaces, Nodes, Pods, Services, Events, Deployments}

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
func (r ResourceInfo) SetTypeMeta(obj Object) {
	t := obj.GetTypeMeta()
	t.APIVersion, t.Kind = r.GroupVersion(), r.Kind
}
