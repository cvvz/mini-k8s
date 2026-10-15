package apiserver_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	authenticationv1 "mini-k8s/pkg/apis/authentication/v1"
	authorizationv1 "mini-k8s/pkg/apis/authorization/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	rbacv1 "mini-k8s/pkg/apis/rbac/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/util/ptr"
)

// tokenFor requests a token of a service account, bound to a Pod if named.
func tokenFor(t *testing.T, c *client.Client, ns, sa string, pod *corev1.Pod) string {
	t.Helper()
	req := authenticationv1.TokenRequest{}
	if pod != nil {
		req.Spec.BoundObjectRef = &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID}
	}
	var resp authenticationv1.TokenRequest
	if err := c.Do(context.Background(), http.MethodPost, "/api/v1/namespaces/"+ns+"/serviceaccounts/"+sa+"/token", nil, "", &req, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status.Token == "" || *resp.Spec.ExpirationSeconds != 3600 || resp.Kind != "TokenRequest" {
		t.Fatalf("token request: %+v", resp)
	}
	return resp.Status.Token
}

func TestServiceAccountsAndRBAC(t *testing.T) {
	admin, base := newServer(t)
	ctx := context.Background()

	// Bootstrap: default service accounts and the default policy.
	if _, err := client.ServiceAccounts(admin, "default").Get(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cluster-admin", "admin", "edit", "view", "system:discovery", "system:basic-user"} {
		if _, err := client.ClusterRoles(admin).Get(ctx, name); err != nil {
			t.Fatalf("ClusterRole %s: %v", name, err)
		}
	}
	// A new namespace gets its default service account at once.
	if _, err := client.Namespaces(admin).Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ServiceAccounts(admin, "team").Get(ctx, "default"); err != nil {
		t.Fatalf("default SA of a new namespace: %v", err)
	}

	if _, err := client.ServiceAccounts(admin, "team").Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "robot"}}); err != nil {
		t.Fatal(err)
	}
	robot := client.New(base, tokenFor(t, admin, "team", "robot", nil))

	// Authenticated, discovery allowed, but nothing else.
	var groups map[string]any
	if err := robot.Do(ctx, http.MethodGet, "/apis", nil, "", nil, &groups); err != nil {
		t.Fatalf("discovery: %v", err)
	}
	_, err := client.Pods(robot, "team").List(ctx, client.ListOptions{})
	if !client.IsForbidden(err) || err.Error() != `pods is forbidden: User "system:serviceaccount:team:robot" cannot list resource "pods" in API group "" in the namespace "team"` {
		t.Fatalf("list pods: %v", err)
	}
	_, err = client.Deployments(robot, "team").Get(ctx, "web")
	if !client.IsForbidden(err) || err.Error() != `deployments.apps "web" is forbidden: User "system:serviceaccount:team:robot" cannot get resource "deployments" in API group "apps" in the namespace "team"` {
		t.Fatalf("get deployment: %v", err)
	}
	if _, err = client.Nodes(robot).List(ctx, client.ListOptions{}); !client.IsForbidden(err) || !strings.HasSuffix(err.Error(), "at the cluster scope") {
		t.Fatalf("list nodes: %v", err)
	}

	// A Role and RoleBinding let it read Pods in its namespace only.
	if _, err := client.Roles(admin, "team").Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "pod-reader"},
		Rules: []rbacv1.PolicyRule{{Verbs: []string{"get", "list", "watch"}, APIGroups: []string{""}, Resources: []string{"pods"}}}}); err != nil {
		t.Fatal(err)
	}
	rb, err := client.RoleBindings(admin, "team").Create(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "read-pods"},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "pod-reader"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "robot", Namespace: "team"}, {Kind: "User", Name: "alice"}}})
	if err != nil {
		t.Fatal(err)
	}
	if rb.Subjects[1].APIGroup != rbacv1.GroupName {
		t.Fatalf("User subjects get the RBAC group: %+v", rb.Subjects)
	}
	if _, err := client.Pods(robot, "team").List(ctx, client.ListOptions{}); err != nil {
		t.Fatalf("list pods after the binding: %v", err)
	}
	if _, err := client.Pods(robot, "default").List(ctx, client.ListOptions{}); !client.IsForbidden(err) {
		t.Fatalf("other namespace: %v", err)
	}
	if err := client.Pods(robot, "team").Delete(ctx, "x", nil); !client.IsForbidden(err) {
		t.Fatalf("delete: %v", err)
	}
	rb.RoleRef.Name = "other"
	if _, err := client.RoleBindings(admin, "team").Update(ctx, rb); !client.IsInvalid(err) || !strings.Contains(err.Error(), "cannot change roleRef") {
		t.Fatalf("roleRef change: %v", err)
	}

	// Self reviews, and impersonation by the admin.
	var who authenticationv1.SelfSubjectReview
	if err := robot.Do(ctx, http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews", nil, "", &who, &who); err != nil ||
		who.Status.UserInfo.Username != "system:serviceaccount:team:robot" {
		t.Fatalf("whoami: %+v %v", who, err)
	}
	review := func(c *client.Client, verb, resource, ns string) authorizationv1.SelfSubjectAccessReview {
		t.Helper()
		r := authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: verb, Resource: resource, Namespace: ns}}}
		if err := c.Do(ctx, http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", nil, "", &r, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := review(admin.Impersonate("system:serviceaccount:team:robot", nil), "list", "pods", "team"); !r.Status.Allowed ||
		r.Status.Reason != `RBAC: allowed by RoleBinding "read-pods/team" of Role "pod-reader" to ServiceAccount "robot/team"` {
		t.Fatalf("can robot list pods: %+v", r.Status)
	}
	if r := review(admin.Impersonate("alice", nil), "delete", "pods", "team"); r.Status.Allowed {
		t.Fatal("alice cannot delete pods")
	}
	if _, err := client.Pods(robot.Impersonate("alice", nil), "team").List(ctx, client.ListOptions{}); !client.IsForbidden(err) ||
		!strings.Contains(err.Error(), `users "alice" is forbidden: User "system:serviceaccount:team:robot" cannot impersonate resource "users"`) {
		t.Fatalf("robot may not impersonate: %v", err)
	}

	// Pods: the service account must exist; its token volume is added.
	_, err = client.Pods(admin, "team").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ghost"},
		Spec: corev1.PodSpec{ServiceAccountName: "ghost", Containers: []corev1.Container{{Name: "c", Image: "img"}}}})
	if !client.IsForbidden(err) || err.Error() != `pods "ghost" is forbidden: error looking up service account team/ghost: serviceaccount "ghost" not found` {
		t.Fatalf("missing service account: %v", err)
	}
	pod, err := client.Pods(admin, "team").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec: corev1.PodSpec{ServiceAccountName: "robot", Containers: []corev1.Container{{Name: "c", Image: "img"}}}})
	if err != nil {
		t.Fatal(err)
	}
	v := pod.Spec.Volumes[len(pod.Spec.Volumes)-1]
	if pod.Spec.ServiceAccount != "robot" || !strings.HasPrefix(v.Name, "kube-api-access-") ||
		*v.Projected.Sources[0].ServiceAccountToken.ExpirationSeconds != 3607 ||
		pod.Spec.Containers[0].VolumeMounts[0].MountPath != "/var/run/secrets/kubernetes.io/serviceaccount" {
		t.Fatalf("token volume: %+v", pod.Spec)
	}
	none, err := client.Pods(admin, "team").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "none"},
		Spec: corev1.PodSpec{AutomountServiceAccountToken: ptr.To(false), Containers: []corev1.Container{{Name: "c", Image: "img"}}}})
	if err != nil || len(none.Spec.Volumes) != 0 || none.Spec.ServiceAccountName != "default" {
		t.Fatalf("automount false: %+v %v", none, err)
	}

	// A token bound to the Pod dies with it; one of a deleted account too.
	bound := client.New(base, tokenFor(t, admin, "team", "robot", pod))
	if _, err := client.Pods(bound, "team").List(ctx, client.ListOptions{}); err != nil {
		t.Fatalf("bound token: %v", err)
	}
	if err := client.Pods(admin, "team").Delete(ctx, "web", &metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Pods(bound, "team").List(ctx, client.ListOptions{}); client.Reason(err) != metav1.ReasonUnauthorized {
		t.Fatalf("token of a deleted Pod: %v", err)
	}
	if err := client.ServiceAccounts(admin, "team").Delete(ctx, "robot", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Pods(robot, "team").List(ctx, client.ListOptions{}); client.Reason(err) != metav1.ReasonUnauthorized {
		t.Fatalf("token of a deleted account: %v", err)
	}
}
