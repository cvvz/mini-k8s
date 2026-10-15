package validation

import (
	"strings"
	"testing"

	rbacv1 "mini-k8s/pkg/apis/rbac/v1"
)

func has(t *testing.T, errs []string, want string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e, want) {
			return
		}
	}
	t.Errorf("errors %q lack %q", errs, want)
}

func TestValidateRules(t *testing.T) {
	ok := []rbacv1.PolicyRule{{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"pods"}}}
	if errs := ValidateRules(ok, true); len(errs) != 0 {
		t.Fatalf("valid: %v", errs)
	}
	has(t, ValidateRules([]rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}}}, true),
		"rules[0].verbs: Required value: verbs must contain at least one value")
	has(t, ValidateRules([]rbacv1.PolicyRule{{Verbs: []string{"get"}, Resources: []string{"pods"}}}, true),
		"rules[0].apiGroups: Required value: resource rules must supply at least one api group")
	has(t, ValidateRules([]rbacv1.PolicyRule{{Verbs: []string{"get"}, NonResourceURLs: []string{"/healthz"}}}, true),
		`rules[0].nonResourceURLs: Invalid value: ["/healthz"]: namespaced rules cannot apply to non-resource URLs`)
	if errs := ValidateRules([]rbacv1.PolicyRule{{Verbs: []string{"get"}, NonResourceURLs: []string{"/healthz"}}}, false); len(errs) != 0 {
		t.Fatalf("cluster non-resource rule: %v", errs)
	}
}

func TestValidateBinding(t *testing.T) {
	ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "r"}
	sa := []rbacv1.Subject{{Kind: "ServiceAccount", Name: "robot"}}
	if errs := ValidateBinding(ref, sa, true); len(errs) != 0 {
		t.Fatalf("valid: %v", errs)
	}
	has(t, ValidateBinding(rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role"}, sa, true), "roleRef.name: Required value")
	has(t, ValidateBinding(ref, sa, false), `roleRef.kind: Unsupported value: "Role": supported values: "ClusterRole"`)
	has(t, ValidateBinding(rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"}, sa, false),
		"subjects[0].namespace: Required value")
	has(t, ValidateBinding(ref, []rbacv1.Subject{{Kind: "Robot", Name: "x"}}, true), `subjects[0].kind: Unsupported value: "Robot"`)
	if errs := ValidateRoleRefUpdate(ref, ref); errs != nil {
		t.Fatal(errs)
	}
	has(t, ValidateRoleRefUpdate(rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "scaler"}, ref),
		`roleRef: Invalid value: {"APIGroup":"rbac.authorization.k8s.io","Kind":"Role","Name":"scaler"}: cannot change roleRef`)
}
