// Package rbac holds the API strategies of the RBAC objects.
package rbac

import (
	metav1 "mini-k8s/pkg/apis/meta/v1"
	rbacv1 "mini-k8s/pkg/apis/rbac/v1"
	"mini-k8s/pkg/apis/rbac/validation"
	"mini-k8s/pkg/apiserver"
)

// RoleStrategy validates Roles.
var RoleStrategy = apiserver.Strategy{
	NameIsPathSegment: true,
	Validate:          func(obj metav1.Object) []string { return validation.ValidateRules(obj.(*rbacv1.Role).Rules, true) },
}

// ClusterRoleStrategy validates ClusterRoles.
var ClusterRoleStrategy = apiserver.Strategy{
	NameIsPathSegment: true,
	Validate: func(obj metav1.Object) []string {
		return validation.ValidateRules(obj.(*rbacv1.ClusterRole).Rules, false)
	},
}

// RoleBindingStrategy defaults subjects, validates and keeps the role.
var RoleBindingStrategy = apiserver.Strategy{
	NameIsPathSegment: true,
	Defaults:          func(obj metav1.Object) { rbacv1.SetDefaultsSubjects(obj.(*rbacv1.RoleBinding).Subjects) },
	Validate: func(obj metav1.Object) []string {
		b := obj.(*rbacv1.RoleBinding)
		return validation.ValidateBinding(b.RoleRef, b.Subjects, true)
	},
	ValidateUpdate: func(obj, old metav1.Object) []string {
		return validation.ValidateRoleRefUpdate(obj.(*rbacv1.RoleBinding).RoleRef, old.(*rbacv1.RoleBinding).RoleRef)
	},
}

// ClusterRoleBindingStrategy defaults subjects, validates and keeps the role.
var ClusterRoleBindingStrategy = apiserver.Strategy{
	NameIsPathSegment: true,
	Defaults:          func(obj metav1.Object) { rbacv1.SetDefaultsSubjects(obj.(*rbacv1.ClusterRoleBinding).Subjects) },
	Validate: func(obj metav1.Object) []string {
		b := obj.(*rbacv1.ClusterRoleBinding)
		return validation.ValidateBinding(b.RoleRef, b.Subjects, false)
	},
	ValidateUpdate: func(obj, old metav1.Object) []string {
		return validation.ValidateRoleRefUpdate(obj.(*rbacv1.ClusterRoleBinding).RoleRef, old.(*rbacv1.ClusterRoleBinding).RoleRef)
	},
}
