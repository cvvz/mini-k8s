// Package bootstrappolicy holds the ClusterRoles and ClusterRoleBindings
// the API server creates at start, a subset of Kubernetes'
// plugin/pkg/auth/authorizer/rbac/bootstrappolicy: cluster-admin for
// system:masters, the admin/edit/view user-facing roles, and discovery
// and self-review for every authenticated user.
package bootstrappolicy

import (
	metav1 "mini-k8s/pkg/apis/meta/v1"
	rbacv1 "mini-k8s/pkg/apis/rbac/v1"
	"mini-k8s/pkg/auth/user"
)

var (
	read  = []string{"get", "list", "watch"}
	write = []string{"create", "delete", "deletecollection", "patch", "update"}
	all   = append(append([]string{}, read...), write...)
)

func rule(verbs []string, group string, resources ...string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{Verbs: verbs, APIGroups: []string{group}, Resources: resources}
}

func meta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Labels: map[string]string{"kubernetes.io/bootstrapping": "rbac-defaults"},
		Annotations: map[string]string{"rbac.authorization.kubernetes.io/autoupdate": "true"}}
}

// viewRules read everything in a namespace but Secrets.
func viewRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		rule(read, "", "pods", "pods/log", "pods/status", "configmaps", "endpoints", "persistentvolumeclaims",
			"persistentvolumeclaims/status", "services", "services/status", "serviceaccounts", "events"),
		rule(read, "", "namespaces", "namespaces/status"),
		rule(read, "apps", "deployments", "deployments/scale", "deployments/status", "replicasets", "replicasets/scale",
			"replicasets/status", "daemonsets", "daemonsets/status", "statefulsets", "statefulsets/scale", "statefulsets/status",
			"controllerrevisions"),
		rule(read, "batch", "jobs", "jobs/status", "cronjobs", "cronjobs/status"),
		rule(read, "autoscaling", "horizontalpodautoscalers", "horizontalpodautoscalers/status"),
		rule(read, "networking.k8s.io", "ingresses", "ingresses/status"),
		rule(read, "metrics.k8s.io", "pods", "nodes"),
	}
}

// editRules change the workloads, configuration and service accounts of a
// namespace, and read its Secrets.
func editRules() []rbacv1.PolicyRule {
	return append(viewRules(),
		rule(all, "", "pods", "pods/exec", "pods/eviction", "configmaps", "endpoints", "persistentvolumeclaims", "services",
			"secrets", "serviceaccounts"),
		rule(read, "", "secrets"),
		rule([]string{"create"}, "", "serviceaccounts/token"),
		rule([]string{"impersonate"}, "", "serviceaccounts"),
		rule(all, "apps", "deployments", "deployments/scale", "replicasets", "replicasets/scale", "daemonsets", "statefulsets",
			"statefulsets/scale"),
		rule(all, "batch", "jobs", "cronjobs"),
		rule(all, "autoscaling", "horizontalpodautoscalers"),
		rule(all, "networking.k8s.io", "ingresses"),
	)
}

// adminRules add the namespace's Roles and RoleBindings.
func adminRules() []rbacv1.PolicyRule {
	return append(editRules(), rule(all, rbacv1.GroupName, "roles", "rolebindings"))
}

// ClusterRoles are created when missing.
func ClusterRoles() []rbacv1.ClusterRole {
	return []rbacv1.ClusterRole{
		{ObjectMeta: meta("cluster-admin"), Rules: []rbacv1.PolicyRule{
			{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}},
			{Verbs: []string{"*"}, NonResourceURLs: []string{"*"}},
		}},
		{ObjectMeta: meta("admin"), Rules: adminRules()},
		{ObjectMeta: meta("edit"), Rules: editRules()},
		{ObjectMeta: meta("view"), Rules: viewRules()},
		{ObjectMeta: meta("system:discovery"), Rules: []rbacv1.PolicyRule{{Verbs: []string{"get"},
			NonResourceURLs: []string{"/api", "/api/*", "/apis", "/apis/*", "/healthz", "/livez", "/readyz", "/version", "/version/"}}}},
		{ObjectMeta: meta("system:basic-user"), Rules: []rbacv1.PolicyRule{
			rule([]string{"create"}, "authorization.k8s.io", "selfsubjectaccessreviews"),
			rule([]string{"create"}, "authentication.k8s.io", "selfsubjectreviews"),
		}},
		{ObjectMeta: meta("system:public-info-viewer"), Rules: []rbacv1.PolicyRule{{Verbs: []string{"get"},
			NonResourceURLs: []string{"/healthz", "/livez", "/readyz", "/version", "/version/"}}}},
	}
}

func binding(name, role string, groups ...string) rbacv1.ClusterRoleBinding {
	b := rbacv1.ClusterRoleBinding{ObjectMeta: meta(name), RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role}}
	for _, g := range groups {
		b.Subjects = append(b.Subjects, rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: g})
	}
	return b
}

// ClusterRoleBindings are created when missing.
func ClusterRoleBindings() []rbacv1.ClusterRoleBinding {
	return []rbacv1.ClusterRoleBinding{
		binding("cluster-admin", "cluster-admin", user.SystemPrivilegedGroup),
		binding("system:discovery", "system:discovery", user.AllAuthenticated),
		binding("system:basic-user", "system:basic-user", user.AllAuthenticated),
		binding("system:public-info-viewer", "system:public-info-viewer", user.AllAuthenticated, user.AllUnauthenticated),
	}
}
