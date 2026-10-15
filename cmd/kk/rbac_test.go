package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	rbacv1 "mini-k8s/pkg/apis/rbac/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/integration/framework"
)

func TestServiceAccountAndRBACCommands(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := context.Background()
	run := func(argv ...string) string {
		t.Helper()
		out, err := kk(t, c, "", argv...)
		if err != nil {
			t.Fatalf("kk %v: %v", argv, err)
		}
		return out
	}
	if out := run("create", "serviceaccount", "robot"); out != "serviceaccount/robot created\n" {
		t.Fatalf("create sa: %q", out)
	}
	if out := run("get", "sa"); !strings.Contains(strings.Join(strings.Fields(out), " "), "NAME AGE default") || !strings.Contains(out, "robot") {
		t.Fatalf("get sa:\n%s", out)
	}
	token := strings.TrimSpace(run("create", "token", "robot", "--duration", "15m"))
	if strings.Count(token, ".") != 2 {
		t.Fatalf("token: %q", token)
	}
	if out := run("create", "role", "pod-reader", "--verb=get,list,watch", "--resource=pods,pods/log", "--resource=deploy/scale"); out !=
		"role.rbac.authorization.k8s.io/pod-reader created\n" {
		t.Fatalf("create role: %q", out)
	}
	role, err := client.Roles(c, "default").Get(ctx, "pod-reader")
	if err != nil {
		t.Fatal(err)
	}
	if len(role.Rules) != 2 || strings.Join(role.Rules[0].Resources, ",") != "pods,pods/log" || role.Rules[1].APIGroups[0] != "apps" ||
		role.Rules[1].Resources[0] != "deployments/scale" {
		t.Fatalf("rules: %+v", role.Rules)
	}
	if out := run("create", "rolebinding", "read-pods", "--role=pod-reader", "--serviceaccount=default:robot", "--user=alice", "--group=devs"); out !=
		"rolebinding.rbac.authorization.k8s.io/read-pods created\n" {
		t.Fatalf("create rolebinding: %q", out)
	}
	if out := run("get", "rolebinding", "-o", "wide"); !strings.Contains(strings.Join(strings.Fields(out), " "),
		"NAME ROLE AGE USERS GROUPS SERVICEACCOUNTS read-pods Role/pod-reader") || !strings.Contains(out, "default/robot") {
		t.Fatalf("get rolebinding:\n%s", out)
	}
	run("create", "clusterrole", "node-reader", "--verb=get,list", "--resource=nodes", "--non-resource-url=/healthz")
	run("create", "clusterrolebinding", "read-nodes", "--clusterrole=node-reader", "--group=ops")
	if out := run("get", "clusterrolebinding", "read-nodes"); !strings.Contains(out, "ClusterRole/node-reader") {
		t.Fatalf("get clusterrolebinding:\n%s", out)
	}
	if out := run("describe", "role", "pod-reader"); !strings.Contains(out, "PolicyRule:") || !strings.Contains(out, "deployments.apps/scale") {
		t.Fatalf("describe role:\n%s", out)
	}
	if out := run("describe", "rolebinding", "read-pods"); !strings.Contains(out, "ServiceAccount") || !strings.Contains(out, "pod-reader") {
		t.Fatalf("describe rolebinding:\n%s", out)
	}

	// auth can-i and whoami, as the service account through impersonation.
	as := []string{"--as", "system:serviceaccount:default:robot"}
	if out := run(append([]string{"auth", "can-i", "list", "pods"}, as...)...); out != "yes\n" {
		t.Fatalf("can-i list pods: %q", out)
	}
	out, err := kk(t, c, "", append([]string{"auth", "can-i", "delete", "pods"}, as...)...)
	var exit silentExit
	if out != "no\n" || !errors.As(err, &exit) || exit.code != 1 {
		t.Fatalf("can-i delete pods: %q %v", out, err)
	}
	if out := run(append([]string{"auth", "can-i", "get", "pods/log"}, as...)...); out != "yes\n" {
		t.Fatalf("can-i get pods/log: %q", out)
	}
	if out := run("auth", "can-i", "list", "nodes", "--as", "bob", "--as-group", "ops"); out != "yes\n" {
		t.Fatalf("can-i through a group: %q", out)
	}
	if out := run(append([]string{"auth", "whoami"}, as...)...); !strings.Contains(out, "system:serviceaccount:default:robot") ||
		!strings.Contains(out, "[system:serviceaccounts system:serviceaccounts:default system:authenticated]") {
		t.Fatalf("whoami:\n%s", out)
	}
	if out := run("auth", "whoami"); !strings.Contains(out, "kubernetes-admin") || !strings.Contains(out, "system:masters") {
		t.Fatalf("admin whoami:\n%s", out)
	}
	if _, err := kk(t, c, "", append([]string{"get", "secrets"}, as...)...); !client.IsForbidden(err) {
		t.Fatalf("get secrets as robot: %v", err)
	}
	if _, err := kk(t, c, "", "create", "rolebinding", "bad", "--role=x", "--clusterrole=y"); err == nil ||
		err.Error() != "exactly one of clusterrole or role must be specified" {
		t.Fatalf("bad binding: %v", err)
	}
	if _, err := client.ClusterRoleBindings(c).Get(ctx, "cluster-admin"); err != nil {
		t.Fatal(err)
	}
	_ = rbacv1.GroupName
}
