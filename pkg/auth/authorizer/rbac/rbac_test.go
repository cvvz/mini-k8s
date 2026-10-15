package rbac

import (
	"context"
	"testing"

	metav1 "mini-k8s/pkg/apis/meta/v1"
	rbacv1 "mini-k8s/pkg/apis/rbac/v1"
	"mini-k8s/pkg/auth/user"
	"mini-k8s/pkg/serviceaccount"
)

type fakeStore struct {
	crbs         []rbacv1.ClusterRoleBinding
	rbs          map[string][]rbacv1.RoleBinding
	clusterRoles map[string]*rbacv1.ClusterRole
	roles        map[string]*rbacv1.Role // by namespace/name
}

func (f fakeStore) ClusterRoleBindings(context.Context) ([]rbacv1.ClusterRoleBinding, error) {
	return f.crbs, nil
}
func (f fakeStore) RoleBindings(_ context.Context, ns string) ([]rbacv1.RoleBinding, error) {
	return f.rbs[ns], nil
}
func (f fakeStore) ClusterRole(_ context.Context, name string) (*rbacv1.ClusterRole, error) {
	return f.clusterRoles[name], nil
}
func (f fakeStore) Role(_ context.Context, ns, name string) (*rbacv1.Role, error) {
	return f.roles[ns+"/"+name], nil
}

func TestAuthorize(t *testing.T) {
	robot := serviceaccount.UserInfo("ns", "robot", "uid")
	alice := &user.Info{Name: "alice", Groups: []string{"devs", user.AllAuthenticated}}
	store := fakeStore{
		crbs: []rbacv1.ClusterRoleBinding{{ObjectMeta: metav1.ObjectMeta{Name: "discovery"},
			RoleRef:  rbacv1.RoleRef{Kind: "ClusterRole", Name: "system:discovery"},
			Subjects: []rbacv1.Subject{{Kind: "Group", Name: user.AllAuthenticated}}}},
		clusterRoles: map[string]*rbacv1.ClusterRole{
			"system:discovery": {ObjectMeta: metav1.ObjectMeta{Name: "system:discovery"},
				Rules: []rbacv1.PolicyRule{{Verbs: []string{"get"}, NonResourceURLs: []string{"/api", "/apis/*"}}}},
			"view": {ObjectMeta: metav1.ObjectMeta{Name: "view"},
				Rules: []rbacv1.PolicyRule{{Verbs: []string{"get", "list"}, APIGroups: []string{"apps"}, Resources: []string{"deployments", "*/scale"}}}},
		},
		roles: map[string]*rbacv1.Role{
			"ns/pod-reader": {ObjectMeta: metav1.ObjectMeta{Name: "pod-reader"},
				Rules: []rbacv1.PolicyRule{{Verbs: []string{"get", "list"}, APIGroups: []string{""}, Resources: []string{"pods", "pods/log"}},
					{Verbs: []string{"update"}, APIGroups: []string{"apps"}, Resources: []string{"deployments/scale"}, ResourceNames: []string{"web"}}}},
		},
		rbs: map[string][]rbacv1.RoleBinding{"ns": {
			{ObjectMeta: metav1.ObjectMeta{Name: "read-pods", Namespace: "ns"}, RoleRef: rbacv1.RoleRef{Kind: "Role", Name: "pod-reader"},
				Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "robot"}, {Kind: "Group", Name: "devs"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "view", Namespace: "ns"}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "view"},
				Subjects: []rbacv1.Subject{{Kind: "User", Name: "alice"}}},
		}},
	}
	a := &Authorizer{Store: store}
	res := func(u *user.Info, verb, group, resource, sub, ns, name string) Attributes {
		return Attributes{User: u, Verb: verb, ResourceRequest: true, APIGroup: group, Resource: resource, Subresource: sub, Namespace: ns, Name: name}
	}
	for _, tc := range []struct {
		attrs  Attributes
		want   bool
		reason string
	}{
		{res(robot, "list", "", "pods", "", "ns", ""), true, `RBAC: allowed by RoleBinding "read-pods/ns" of Role "pod-reader" to ServiceAccount "robot/ns"`},
		{res(robot, "get", "", "pods", "log", "ns", "web"), true, ""},
		{res(robot, "delete", "", "pods", "", "ns", "web"), false, ""},
		{res(robot, "list", "", "pods", "", "other", ""), false, ""},
		{res(robot, "list", "", "secrets", "", "ns", ""), false, ""},
		{res(robot, "update", "apps", "deployments", "scale", "ns", "web"), true, ""},
		{res(robot, "update", "apps", "deployments", "scale", "ns", "db"), false, ""},
		{res(alice, "get", "", "pods", "", "ns", "x"), true, ""}, // through the devs group
		{res(alice, "get", "apps", "statefulsets", "scale", "ns", "x"), true, ""},
		{res(alice, "get", "apps", "deployments", "", "", ""), false, ""}, // a RoleBinding is not cluster-wide
		{Attributes{User: robot, Verb: "get", Path: "/apis/apps/v1"}, true, ""},
		{Attributes{User: robot, Verb: "get", Path: "/version"}, false, ""},
		{res(&user.Info{Name: "admin", Groups: []string{user.SystemPrivilegedGroup}}, "delete", "", "nodes", "", "", "n1"), true, ""},
	} {
		got, reason, err := a.Authorize(context.Background(), tc.attrs)
		if err != nil || got != tc.want || (tc.reason != "" && reason != tc.reason) {
			t.Errorf("%+v: %v %q %v, want %v %q", tc.attrs, got, reason, err, tc.want, tc.reason)
		}
	}
}

func TestForbiddenMessage(t *testing.T) {
	robot := serviceaccount.UserInfo("ns", "robot", "uid")
	for _, tc := range []struct {
		attrs Attributes
		want  string
	}{
		{Attributes{User: robot, Verb: "list", ResourceRequest: true, Resource: "pods", Namespace: "ns"},
			`User "system:serviceaccount:ns:robot" cannot list resource "pods" in API group "" in the namespace "ns"`},
		{Attributes{User: robot, Verb: "list", ResourceRequest: true, Resource: "nodes"},
			`User "system:serviceaccount:ns:robot" cannot list resource "nodes" in API group "" at the cluster scope`},
		{Attributes{User: robot, Verb: "create", ResourceRequest: true, Resource: "pods", Subresource: "exec", Namespace: "ns", Name: "web"},
			`User "system:serviceaccount:ns:robot" cannot create resource "pods/exec" in API group "" in the namespace "ns"`},
		{Attributes{User: robot, Verb: "get", Path: "/version"}, `User "system:serviceaccount:ns:robot" cannot get path "/version"`},
	} {
		if got := ForbiddenMessage(tc.attrs); got != tc.want {
			t.Errorf("%q, want %q", got, tc.want)
		}
	}
}
