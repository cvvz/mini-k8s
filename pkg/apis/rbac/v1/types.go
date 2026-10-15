// Package v1 contains the rbac.authorization.k8s.io/v1 types (rbacv1):
// Roles and ClusterRoles list permitted actions, RoleBindings and
// ClusterRoleBindings grant them to users, groups and service accounts.
package v1

import metav1 "mini-k8s/pkg/apis/meta/v1"

// GroupName is the API group of RBAC.
const GroupName = "rbac.authorization.k8s.io"

// Subject kinds.
const (
	UserKind           = "User"
	GroupKind          = "Group"
	ServiceAccountKind = "ServiceAccount"
)

// PolicyRule allows verbs on resources (or non-resource URLs).
type PolicyRule struct {
	Verbs           []string `json:"verbs"`
	APIGroups       []string `json:"apiGroups,omitempty"`
	Resources       []string `json:"resources,omitempty"`
	ResourceNames   []string `json:"resourceNames,omitempty"`
	NonResourceURLs []string `json:"nonResourceURLs,omitempty"`
}

// Subject is who a binding grants a role to.
type Subject struct {
	Kind      string `json:"kind"`
	APIGroup  string `json:"apiGroup,omitempty"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// RoleRef names the granted Role or ClusterRole.
type RoleRef struct {
	APIGroup string `json:"apiGroup"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
}

// Role grants rules within its namespace.
type Role struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Rules             []PolicyRule `json:"rules"`
}

// ClusterRole grants rules cluster-wide, or in a namespace through a
// RoleBinding.
type ClusterRole struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Rules             []PolicyRule `json:"rules"`
}

// RoleBinding grants a Role or ClusterRole within its namespace.
type RoleBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Subjects          []Subject `json:"subjects,omitempty"`
	RoleRef           RoleRef   `json:"roleRef"`
}

// ClusterRoleBinding grants a ClusterRole cluster-wide.
type ClusterRoleBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Subjects          []Subject `json:"subjects,omitempty"`
	RoleRef           RoleRef   `json:"roleRef"`
}

// SetDefaultsSubjects gives User and Group subjects the RBAC API group, as
// the Kubernetes defaulting does.
func SetDefaultsSubjects(subjects []Subject) {
	for i := range subjects {
		if s := &subjects[i]; s.APIGroup == "" && (s.Kind == UserKind || s.Kind == GroupKind) {
			s.APIGroup = GroupName
		}
	}
}
