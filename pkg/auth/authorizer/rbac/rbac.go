// Package rbac authorizes requests with Roles, ClusterRoles and their
// bindings, like k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac:
// members of system:masters may do anything; anyone else needs a rule of
// a role bound to them (a ClusterRoleBinding anywhere, a RoleBinding in
// the request's namespace) that matches the verb, API group, resource,
// subresource and name, or the non-resource path.
package rbac

import (
	"context"
	"fmt"
	"strings"

	rbacv1 "mini-k8s/pkg/apis/rbac/v1"
	"mini-k8s/pkg/auth/user"
	"mini-k8s/pkg/serviceaccount"
)

// Attributes describe a request.
type Attributes struct {
	User *user.Info
	// Verb is get, list, watch, create, update, patch, delete,
	// deletecollection, impersonate... (or an HTTP verb for paths).
	Verb            string
	ResourceRequest bool
	Namespace       string
	APIGroup        string
	Resource        string
	Subresource     string
	Name            string
	// Path is the URL path of a non-resource request.
	Path string
}

// Store reads the RBAC objects.
type Store interface {
	ClusterRoleBindings(ctx context.Context) ([]rbacv1.ClusterRoleBinding, error)
	RoleBindings(ctx context.Context, namespace string) ([]rbacv1.RoleBinding, error)
	// ClusterRole and Role return nil for a missing role.
	ClusterRole(ctx context.Context, name string) (*rbacv1.ClusterRole, error)
	Role(ctx context.Context, namespace, name string) (*rbacv1.Role, error)
}

// Authorizer decides with the RBAC objects of a Store.
type Authorizer struct{ Store Store }

// Authorize reports whether a request is allowed and, if so, why.
func (a *Authorizer) Authorize(ctx context.Context, attrs Attributes) (bool, string, error) {
	if attrs.User == nil {
		return false, "", nil
	}
	if attrs.User.InGroup(user.SystemPrivilegedGroup) {
		return true, "", nil
	}
	crbs, err := a.Store.ClusterRoleBindings(ctx)
	if err != nil {
		return false, "", err
	}
	for _, b := range crbs {
		subject, ok := appliesTo(attrs.User, b.Subjects, "")
		if !ok || b.RoleRef.Kind != "ClusterRole" {
			continue
		}
		role, err := a.Store.ClusterRole(ctx, b.RoleRef.Name)
		if err != nil || role == nil {
			continue
		}
		if rulesAllow(attrs, role.Rules) {
			return true, fmt.Sprintf("RBAC: allowed by ClusterRoleBinding %q of ClusterRole %q to %s", b.Name, role.Name, describeSubject(subject, "")), nil
		}
	}
	if attrs.Namespace == "" {
		return false, "", nil
	}
	rbs, err := a.Store.RoleBindings(ctx, attrs.Namespace)
	if err != nil {
		return false, "", err
	}
	for _, b := range rbs {
		subject, ok := appliesTo(attrs.User, b.Subjects, b.Namespace)
		if !ok {
			continue
		}
		var rules []rbacv1.PolicyRule
		switch b.RoleRef.Kind {
		case "Role":
			role, err := a.Store.Role(ctx, b.Namespace, b.RoleRef.Name)
			if err != nil || role == nil {
				continue
			}
			rules = role.Rules
		case "ClusterRole":
			role, err := a.Store.ClusterRole(ctx, b.RoleRef.Name)
			if err != nil || role == nil {
				continue
			}
			rules = role.Rules
		}
		if rulesAllow(attrs, rules) {
			return true, fmt.Sprintf("RBAC: allowed by RoleBinding \"%s/%s\" of %s %q to %s", b.Name, b.Namespace, b.RoleRef.Kind,
				b.RoleRef.Name, describeSubject(subject, b.Namespace)), nil
		}
	}
	return false, "", nil
}

func describeSubject(s *rbacv1.Subject, bindingNamespace string) string {
	if s.Kind == rbacv1.ServiceAccountKind {
		ns := s.Namespace
		if ns == "" {
			ns = bindingNamespace
		}
		return fmt.Sprintf("%s \"%s/%s\"", s.Kind, s.Name, ns)
	}
	return fmt.Sprintf("%s %q", s.Kind, s.Name)
}

// appliesTo finds the subject that names the user.
func appliesTo(u *user.Info, subjects []rbacv1.Subject, bindingNamespace string) (*rbacv1.Subject, bool) {
	for i := range subjects {
		s := &subjects[i]
		switch s.Kind {
		case rbacv1.UserKind:
			if u.Name == s.Name {
				return s, true
			}
		case rbacv1.GroupKind:
			if u.InGroup(s.Name) {
				return s, true
			}
		case rbacv1.ServiceAccountKind:
			ns := s.Namespace
			if ns == "" {
				ns = bindingNamespace
			}
			if ns != "" && u.Name == serviceaccount.MakeUsername(ns, s.Name) {
				return s, true
			}
		}
	}
	return nil, false
}

func rulesAllow(attrs Attributes, rules []rbacv1.PolicyRule) bool {
	for _, r := range rules {
		if RuleAllows(attrs, r) {
			return true
		}
	}
	return false
}

// RuleAllows matches one rule against a request.
func RuleAllows(attrs Attributes, r rbacv1.PolicyRule) bool {
	if !contains(r.Verbs, attrs.Verb) {
		return false
	}
	if !attrs.ResourceRequest {
		for _, u := range r.NonResourceURLs {
			if u == "*" || u == attrs.Path || (strings.HasSuffix(u, "*") && strings.HasPrefix(attrs.Path, strings.TrimSuffix(u, "*"))) {
				return true
			}
		}
		return false
	}
	if !contains(r.APIGroups, attrs.APIGroup) || !resourceMatches(r.Resources, attrs.Resource, attrs.Subresource) {
		return false
	}
	if len(r.ResourceNames) == 0 {
		return true
	}
	for _, n := range r.ResourceNames {
		if attrs.Name != "" && n == attrs.Name {
			return true
		}
	}
	return false
}

// contains matches a value or the "*" wildcard.
func contains(list []string, v string) bool {
	for _, x := range list {
		if x == "*" || x == v {
			return true
		}
	}
	return false
}

func resourceMatches(resources []string, resource, subresource string) bool {
	combined := resource
	if subresource != "" {
		combined += "/" + subresource
	}
	for _, r := range resources {
		switch {
		case r == "*", r == combined:
			return true
		case subresource != "" && r == "*/"+subresource:
			return true
		}
	}
	return false
}

// ForbiddenMessage is the reason of a denial, as Kubernetes words it.
func ForbiddenMessage(attrs Attributes) string {
	name := ""
	if attrs.User != nil {
		name = attrs.User.Name
	}
	if !attrs.ResourceRequest {
		return fmt.Sprintf("User %q cannot %s path %q", name, attrs.Verb, attrs.Path)
	}
	resource := attrs.Resource
	if attrs.Subresource != "" {
		resource += "/" + attrs.Subresource
	}
	if attrs.Namespace != "" {
		return fmt.Sprintf("User %q cannot %s resource %q in API group %q in the namespace %q", name, attrs.Verb, resource,
			attrs.APIGroup, attrs.Namespace)
	}
	return fmt.Sprintf("User %q cannot %s resource %q in API group %q at the cluster scope", name, attrs.Verb, resource, attrs.APIGroup)
}
