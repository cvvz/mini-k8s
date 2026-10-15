// Package validation checks RBAC objects like Kubernetes.
package validation

import (
	"encoding/json"
	"fmt"

	rbacv1 "mini-k8s/pkg/apis/rbac/v1"
)

// ValidateRules checks the rules of a Role (namespaced) or ClusterRole.
func ValidateRules(rules []rbacv1.PolicyRule, namespaced bool) []string {
	var errs []string
	for i, r := range rules {
		path := fmt.Sprintf("rules[%d]", i)
		if len(r.Verbs) == 0 {
			errs = append(errs, path+".verbs: Required value: verbs must contain at least one value")
		}
		if len(r.NonResourceURLs) > 0 {
			urls, _ := json.Marshal(r.NonResourceURLs)
			if namespaced {
				errs = append(errs, fmt.Sprintf("%s.nonResourceURLs: Invalid value: %s: namespaced rules cannot apply to non-resource URLs", path, urls))
			}
			if len(r.APIGroups) > 0 || len(r.Resources) > 0 {
				errs = append(errs, fmt.Sprintf("%s.nonResourceURLs: Invalid value: %s: rules cannot apply to both regular resources and non-resource URLs", path, urls))
			}
			continue
		}
		if len(r.APIGroups) == 0 {
			errs = append(errs, path+".apiGroups: Required value: resource rules must supply at least one api group")
		}
		if len(r.Resources) == 0 {
			errs = append(errs, path+".resources: Required value: resource rules must supply at least one resource")
		}
	}
	return errs
}

// ValidateBinding checks a RoleBinding (namespaced) or ClusterRoleBinding.
func ValidateBinding(roleRef rbacv1.RoleRef, subjects []rbacv1.Subject, namespaced bool) []string {
	var errs []string
	if roleRef.APIGroup != rbacv1.GroupName {
		errs = append(errs, fmt.Sprintf(`roleRef.apiGroup: Unsupported value: %q: supported values: "rbac.authorization.k8s.io"`, roleRef.APIGroup))
	}
	switch {
	case roleRef.Kind == "ClusterRole":
	case roleRef.Kind == "Role" && namespaced:
	case namespaced:
		errs = append(errs, fmt.Sprintf(`roleRef.kind: Unsupported value: %q: supported values: "Role", "ClusterRole"`, roleRef.Kind))
	default:
		errs = append(errs, fmt.Sprintf(`roleRef.kind: Unsupported value: %q: supported values: "ClusterRole"`, roleRef.Kind))
	}
	if roleRef.Name == "" {
		errs = append(errs, "roleRef.name: Required value")
	}
	for i, s := range subjects {
		path := fmt.Sprintf("subjects[%d]", i)
		if s.Name == "" {
			errs = append(errs, path+".name: Required value")
		}
		switch s.Kind {
		case rbacv1.ServiceAccountKind:
			if s.APIGroup != "" {
				errs = append(errs, fmt.Sprintf(`%s.apiGroup: Unsupported value: %q: supported values: ""`, path, s.APIGroup))
			}
			if !namespaced && s.Namespace == "" {
				errs = append(errs, path+".namespace: Required value")
			}
		case rbacv1.UserKind, rbacv1.GroupKind:
			if s.APIGroup != rbacv1.GroupName {
				errs = append(errs, fmt.Sprintf(`%s.apiGroup: Unsupported value: %q: supported values: "rbac.authorization.k8s.io"`, path, s.APIGroup))
			}
		default:
			errs = append(errs, fmt.Sprintf(`%s.kind: Unsupported value: %q: supported values: "ServiceAccount", "User", "Group"`, path, s.Kind))
		}
	}
	return errs
}

// ValidateRoleRefUpdate keeps the role of a binding.
func ValidateRoleRefUpdate(roleRef, old rbacv1.RoleRef) []string {
	if roleRef == old {
		return nil
	}
	return []string{fmt.Sprintf(`roleRef: Invalid value: {"APIGroup":%q,"Kind":%q,"Name":%q}: cannot change roleRef`, roleRef.APIGroup,
		roleRef.Kind, roleRef.Name)}
}
