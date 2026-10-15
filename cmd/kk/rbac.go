package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	authenticationv1 "mini-k8s/pkg/apis/authentication/v1"
	authorizationv1 "mini-k8s/pkg/apis/authorization/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	rbacv1 "mini-k8s/pkg/apis/rbac/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/scheme"
)

// silentExit ends kk with a code and no message (kk auth can-i says "no"
// and exits 1, like kubectl).
type silentExit struct{ code int }

func (e silentExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func splitList(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// createServiceAccount implements kk create serviceaccount NAME.
func (a *app) createServiceAccount(ctx context.Context, p args) error {
	if len(p.positional) != 2 {
		return errors.New("usage: kk create serviceaccount NAME")
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: p.positional[1], Namespace: a.namespace}}
	if _, err := client.ServiceAccounts(a.client, a.namespace).Create(ctx, sa); err != nil {
		return createError("serviceaccount", err)
	}
	_, err := fmt.Fprintf(a.out, "serviceaccount/%s created\n", sa.Name)
	return err
}

// createToken implements kk create token SERVICEACCOUNT [--duration D]:
// it prints a token of the account (TokenRequest).
func (a *app) createToken(ctx context.Context, p args) error {
	if len(p.positional) != 2 {
		return errors.New("usage: kk create token SERVICEACCOUNT [--duration DURATION]")
	}
	var req authenticationv1.TokenRequest
	if d := p.value("duration", ""); d != "" {
		dur, err := time.ParseDuration(d)
		if err != nil {
			return fmt.Errorf("invalid --duration %q", d)
		}
		secs := int64(dur / time.Second)
		req.Spec.ExpirationSeconds = &secs
	}
	var resp authenticationv1.TokenRequest
	path := scheme.ServiceAccounts.Path(a.namespace, p.positional[1]) + "/token"
	if err := a.client.Do(ctx, http.MethodPost, path, nil, "", &req, &resp); err != nil {
		return err
	}
	_, err := fmt.Fprintln(a.out, resp.Status.Token)
	return err
}

// policyRules turns --verb, --resource and --resource-name into rules,
// one per API group, like kubectl create role.
func policyRules(p args) ([]rbacv1.PolicyRule, error) {
	verbs := splitList(p.values["verb"])
	if len(verbs) == 0 {
		return nil, errors.New("at least one verb must be specified")
	}
	names := splitList(p.values["resource-name"])
	var rules []rbacv1.PolicyRule
	byGroup := map[string]int{}
	for _, r := range splitList(p.values["resource"]) {
		base, sub, _ := strings.Cut(r, "/")
		group := ""
		resource := base
		if res, ok := scheme.Lookup(base); ok {
			group, resource = res.Group, res.Resource
		} else if b, g, ok := strings.Cut(base, "."); ok {
			resource, group = b, g
		} else if base != "*" {
			return nil, fmt.Errorf("the server doesn't have a resource type %q", base)
		}
		if sub != "" {
			resource += "/" + sub
		}
		i, ok := byGroup[group]
		if !ok {
			i = len(rules)
			byGroup[group] = i
			rules = append(rules, rbacv1.PolicyRule{Verbs: verbs, APIGroups: []string{group}, ResourceNames: names})
		}
		rules[i].Resources = append(rules[i].Resources, resource)
	}
	if urls := splitList(p.values["non-resource-url"]); len(urls) > 0 {
		rules = append(rules, rbacv1.PolicyRule{Verbs: verbs, NonResourceURLs: urls})
	}
	if len(rules) == 0 {
		return nil, errors.New("at least one resource must be specified")
	}
	return rules, nil
}

// createRole implements kk create role|clusterrole NAME --verb ... --resource ....
func (a *app) createRole(ctx context.Context, p args, cluster bool) error {
	kind := map[bool]string{false: "role", true: "clusterrole"}[cluster]
	if len(p.positional) != 2 {
		return fmt.Errorf("usage: kk create %s NAME --verb=VERB[,VERB] --resource=RESOURCE[,RESOURCE] [--resource-name=NAME]", kind)
	}
	if !cluster && len(p.values["non-resource-url"]) > 0 {
		return errors.New("--non-resource-url is only valid for a clusterrole")
	}
	rules, err := policyRules(p)
	if err != nil {
		return err
	}
	name := p.positional[1]
	if cluster {
		_, err = client.ClusterRoles(a.client).Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: rules})
	} else {
		_, err = client.Roles(a.client, a.namespace).Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.namespace},
			Rules: rules})
	}
	if err != nil {
		return createError(kind, err)
	}
	_, err = fmt.Fprintf(a.out, "%s.rbac.authorization.k8s.io/%s created\n", kind, name)
	return err
}

// bindingSubjects reads --user, --group and --serviceaccount NS:NAME.
func bindingSubjects(p args) ([]rbacv1.Subject, error) {
	var subjects []rbacv1.Subject
	for _, u := range p.values["user"] {
		subjects = append(subjects, rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: u})
	}
	for _, g := range p.values["group"] {
		subjects = append(subjects, rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: g})
	}
	for _, sa := range p.values["serviceaccount"] {
		ns, name, ok := strings.Cut(sa, ":")
		if !ok || ns == "" || name == "" {
			return nil, fmt.Errorf("serviceaccount must be <namespace>:<name>")
		}
		subjects = append(subjects, rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: ns, Name: name})
	}
	return subjects, nil
}

// createBinding implements kk create rolebinding|clusterrolebinding.
func (a *app) createBinding(ctx context.Context, p args, cluster bool) error {
	kind := map[bool]string{false: "rolebinding", true: "clusterrolebinding"}[cluster]
	if len(p.positional) != 2 {
		return fmt.Errorf("usage: kk create %s NAME --clusterrole=NAME|--role=NAME [--user=USER] [--group=GROUP] [--serviceaccount=NS:NAME]", kind)
	}
	role, clusterRole := p.value("role", ""), p.value("clusterrole", "")
	switch {
	case role != "" && clusterRole != "":
		return errors.New("exactly one of clusterrole or role must be specified")
	case role == "" && clusterRole == "":
		return errors.New("exactly one of clusterrole or role must be specified")
	case cluster && role != "":
		return errors.New("a clusterrolebinding can only refer to a clusterrole")
	}
	ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole}
	if role != "" {
		ref = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role}
	}
	subjects, err := bindingSubjects(p)
	if err != nil {
		return err
	}
	name := p.positional[1]
	if cluster {
		_, err = client.ClusterRoleBindings(a.client).Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name},
			RoleRef: ref, Subjects: subjects})
	} else {
		_, err = client.RoleBindings(a.client, a.namespace).Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.namespace}, RoleRef: ref, Subjects: subjects})
	}
	if err != nil {
		return createError(kind, err)
	}
	_, err = fmt.Fprintf(a.out, "%s.rbac.authorization.k8s.io/%s created\n", kind, name)
	return err
}

const authUsage = "usage: kk auth can-i VERB TYPE[/NAME] [NAME] [--subresource SUB] | kk auth can-i VERB /PATH | kk auth whoami"

// auth implements kk auth can-i and kk auth whoami.
func (a *app) auth(ctx context.Context, p args) error {
	if len(p.positional) == 0 {
		return errors.New(authUsage)
	}
	switch p.positional[0] {
	case "whoami":
		var review authenticationv1.SelfSubjectReview
		if err := a.client.Do(ctx, http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews", nil, "",
			&authenticationv1.SelfSubjectReview{}, &review); err != nil {
			return err
		}
		tw := newTabWriter(a.out)
		fmt.Fprintln(tw, "ATTRIBUTE\tVALUE")
		fmt.Fprintf(tw, "Username\t%s\n", review.Status.UserInfo.Username)
		if uid := review.Status.UserInfo.UID; uid != "" {
			fmt.Fprintf(tw, "UID\t%s\n", uid)
		}
		fmt.Fprintf(tw, "Groups\t[%s]\n", strings.Join(review.Status.UserInfo.Groups, " "))
		return tw.Flush()
	case "can-i":
		return a.canI(ctx, p.positional[1:], p)
	}
	return errors.New(authUsage)
}

func (a *app) canI(ctx context.Context, pos []string, p args) error {
	if len(pos) < 2 || len(pos) > 3 {
		return errors.New(authUsage)
	}
	review := authorizationv1.SelfSubjectAccessReview{}
	verb, target := pos[0], pos[1]
	if strings.HasPrefix(target, "/") {
		review.Spec.NonResourceAttributes = &authorizationv1.NonResourceAttributes{Verb: verb, Path: target}
	} else {
		typ, rest, _ := strings.Cut(target, "/")
		sub, name := p.value("subresource", ""), ""
		res, ok := scheme.Lookup(typ)
		// "pods/exec" names a subresource, "pods/web" a Pod.
		if ok && isSubresource(res, rest) {
			sub = rest
		} else {
			name = rest
		}
		if len(pos) == 3 {
			name = pos[2]
		}
		group, resource := "", typ
		if ok {
			group, resource = res.Group, res.Resource
		} else if r, g, found := strings.Cut(typ, "."); found {
			resource, group = r, g
		} else if typ != "*" {
			fmt.Fprintf(a.errOut, "Warning: the server doesn't have a resource type '%s'\n", typ)
		}
		ns := a.namespace
		if a.allNamespaces || (ok && !res.Namespaced) {
			ns = ""
		}
		review.Spec.ResourceAttributes = &authorizationv1.ResourceAttributes{Namespace: ns, Verb: verb, Group: group, Resource: resource,
			Subresource: sub, Name: name}
	}
	var out authorizationv1.SelfSubjectAccessReview
	if err := a.client.Do(ctx, http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", nil, "", &review, &out); err != nil {
		return err
	}
	if out.Status.Allowed {
		_, err := fmt.Fprintln(a.out, "yes")
		return err
	}
	fmt.Fprintln(a.out, "no")
	return silentExit{code: 1}
}

// isSubresource tells the subresource names kk knows of a resource.
func isSubresource(res scheme.ResourceInfo, name string) bool {
	switch name {
	case "status":
		return res.HasStatus
	case "scale":
		return res.Resource == scheme.Deployments.Resource || res.Resource == scheme.ReplicaSets.Resource ||
			res.Resource == scheme.StatefulSets.Resource
	case "log", "exec", "binding", "eviction":
		return res.Resource == scheme.Pods.Resource
	case "token":
		return res.Resource == scheme.ServiceAccounts.Resource
	case "proxy":
		return res.Resource == scheme.Nodes.Resource
	}
	return false
}

// describeRules writes the PolicyRule table of describe role|clusterrole.
func describeRules(w io.Writer, rules []rbacv1.PolicyRule) {
	type row struct{ resource, urls, names, verbs string }
	var rows []row
	list := func(v []string) string { return "[" + strings.Join(v, " ") + "]" }
	for _, r := range rules {
		if len(r.NonResourceURLs) > 0 {
			rows = append(rows, row{"", list(r.NonResourceURLs), "[]", list(r.Verbs)})
			continue
		}
		for _, res := range r.Resources {
			for _, g := range r.APIGroups {
				name := res
				if g != "" {
					// deployments.apps/scale, as kubectl writes it
					base, sub, hasSub := strings.Cut(res, "/")
					name = base + "." + g
					if hasSub {
						name += "/" + sub
					}
				}
				rows = append(rows, row{name, "[]", list(r.ResourceNames), list(r.Verbs)})
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].resource > rows[j].resource })
	fmt.Fprintln(w, "PolicyRule:")
	fmt.Fprintln(w, "  Resources\tNon-Resource URLs\tResource Names\tVerbs")
	fmt.Fprintln(w, "  ---------\t-----------------\t--------------\t-----")
	for _, r := range rows {
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", r.resource, r.urls, r.names, r.verbs)
	}
}

// describeBinding writes the Role and Subjects of describe rolebinding.
func describeBinding(w io.Writer, ref rbacv1.RoleRef, subjects []rbacv1.Subject) {
	fmt.Fprintln(w, "Role:")
	fmt.Fprintf(w, "  Kind:\t%s\n", ref.Kind)
	fmt.Fprintf(w, "  Name:\t%s\n", ref.Name)
	fmt.Fprintln(w, "Subjects:")
	fmt.Fprintln(w, "  Kind\tName\tNamespace")
	fmt.Fprintln(w, "  ----\t----\t---------")
	for _, s := range subjects {
		fmt.Fprintf(w, "  %s\t%s\t%s\n", s.Kind, s.Name, s.Namespace)
	}
}

// bindingSubjectColumns are the USERS, GROUPS and SERVICEACCOUNTS columns.
func bindingSubjectColumns(subjects []rbacv1.Subject) (users, groups, accounts string) {
	var u, g, sa []string
	for _, s := range subjects {
		switch s.Kind {
		case rbacv1.UserKind:
			u = append(u, s.Name)
		case rbacv1.GroupKind:
			g = append(g, s.Name)
		case rbacv1.ServiceAccountKind:
			sa = append(sa, s.Namespace+"/"+s.Name)
		}
	}
	return strings.Join(u, ", "), strings.Join(g, ", "), strings.Join(sa, ", ")
}
