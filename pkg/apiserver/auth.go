package apiserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	authenticationv1 "mini-k8s/pkg/apis/authentication/v1"
	authorizationv1 "mini-k8s/pkg/apis/authorization/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	rbacv1 "mini-k8s/pkg/apis/rbac/v1"
	"mini-k8s/pkg/auth/authorizer/rbac"
	"mini-k8s/pkg/auth/bootstrappolicy"
	"mini-k8s/pkg/auth/user"
	"mini-k8s/pkg/scheme"
	"mini-k8s/pkg/serviceaccount"
	"mini-k8s/pkg/storage/etcd3"
)

type userKey struct{}

func withUser(ctx context.Context, u *user.Info) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

// userFrom returns the requester of a request's context.
func userFrom(ctx context.Context) *user.Info {
	u, _ := ctx.Value(userKey{}).(*user.Info)
	return u
}

// authenticate returns who sent a request: the cluster token is the
// admin (system:masters); a service account token is its account, while
// the account (and the Pod it is bound to) exists.
func (s *Server) authenticate(r *http.Request) (*user.Info, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return nil, errors.New("no bearer token")
	}
	if s.cfg.Token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Token)) == 1 {
		return &user.Info{Name: user.AdminName, Groups: []string{user.SystemPrivilegedGroup, user.AllAuthenticated}}, nil
	}
	if s.saKey == nil {
		return nil, errors.New("service account tokens are not enabled")
	}
	claims, err := serviceaccount.Verify(s.saKey, token, time.Now(), serviceaccount.Issuer)
	if err != nil {
		return nil, err
	}
	k := claims.Kubernetes
	obj, err := s.get(r.Context(), scheme.ServiceAccounts, k.Namespace, k.ServiceAccount.Name)
	if err != nil || obj.GetObjectMeta().UID != k.ServiceAccount.UID {
		return nil, fmt.Errorf("service account %s/%s has been deleted", k.Namespace, k.ServiceAccount.Name)
	}
	if p := k.Pod; p != nil {
		obj, err := s.get(r.Context(), scheme.Pods, k.Namespace, p.Name)
		if err != nil || obj.GetObjectMeta().UID != p.UID {
			return nil, fmt.Errorf("pod %s/%s has been deleted", k.Namespace, p.Name)
		}
	}
	return serviceaccount.UserInfo(k.Namespace, k.ServiceAccount.Name, k.ServiceAccount.UID), nil
}

// impersonate switches to the identity of the Impersonate-User and
// Impersonate-Group headers when the requester may impersonate it.
func (s *Server) impersonate(r *http.Request, requester *user.Info) (*user.Info, error) {
	name, groups := r.Header.Get("Impersonate-User"), r.Header.Values("Impersonate-Group")
	if name == "" && len(groups) == 0 {
		return requester, nil
	}
	if name == "" {
		return nil, errBadRequest("requested %v without impersonating a user", groups)
	}
	check := func(attrs rbac.Attributes) error {
		attrs.User, attrs.Verb, attrs.ResourceRequest = requester, "impersonate", true
		allowed, _, err := s.authorizer.Authorize(r.Context(), attrs)
		if err != nil {
			return err
		}
		if !allowed {
			return forbidden(attrs)
		}
		return nil
	}
	ns, sa, isSA := serviceaccount.SplitUsername(name)
	if isSA {
		if err := check(rbac.Attributes{Resource: "serviceaccounts", Namespace: ns, Name: sa}); err != nil {
			return nil, err
		}
	} else if err := check(rbac.Attributes{Resource: "users", Name: name}); err != nil {
		return nil, err
	}
	for _, g := range groups {
		if err := check(rbac.Attributes{Resource: "groups", Name: g}); err != nil {
			return nil, err
		}
	}
	if isSA && len(groups) == 0 {
		groups = serviceaccount.MakeGroupNames(ns)
	}
	if !slices.Contains(groups, user.AllAuthenticated) {
		groups = append(groups, user.AllAuthenticated)
	}
	return &user.Info{Name: name, UID: r.Header.Get("Impersonate-Uid"), Groups: groups}, nil
}

// requestAttributes describes a request for the authorizer, as
// Kubernetes' RequestInfoFactory: /api/v1/... and /apis/GROUP/VERSION/...
// with a resource are resource requests, everything else (discovery,
// /version) is a non-resource request.
func requestAttributes(r *http.Request, u *user.Info) rbac.Attributes {
	path := strings.TrimSuffix(r.URL.Path, "/")
	a := rbac.Attributes{User: u, Path: r.URL.Path, Verb: strings.ToLower(r.Method)}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	var rest []string
	switch {
	case len(segs) >= 3 && segs[0] == "api":
		rest = segs[2:]
	case len(segs) >= 4 && segs[0] == "apis":
		a.APIGroup, rest = segs[1], segs[3:]
	default:
		return a
	}
	a.ResourceRequest = true
	if rest[0] == "namespaces" && len(rest) > 1 {
		a.Namespace = rest[1]
		if len(rest) > 2 && rest[2] != "status" && rest[2] != "finalize" {
			rest = rest[2:]
		}
	}
	a.Resource = rest[0]
	if len(rest) > 1 {
		a.Name = rest[1]
	}
	if len(rest) > 2 {
		a.Subresource = rest[2]
	}
	if a.Resource == "namespaces" && a.Name == "" && a.Namespace != "" {
		a.Name = a.Namespace
	}
	watch := r.URL.Query().Get("watch")
	switch r.Method {
	case http.MethodGet:
		switch {
		case watch == "true" || watch == "1":
			a.Verb = "watch"
		case a.Name == "":
			a.Verb = "list"
		default:
			a.Verb = "get"
		}
		if a.Subresource == "proxy" {
			a.Verb = "get"
		}
	case http.MethodPost:
		a.Verb = "create"
	case http.MethodPut:
		a.Verb = "update"
	case http.MethodPatch:
		a.Verb = "patch"
	case http.MethodDelete:
		a.Verb = "delete"
		if a.Name == "" {
			a.Verb = "deletecollection"
		}
	}
	return a
}

// forbidden renders a denial as Kubernetes does.
func forbidden(a rbac.Attributes) *statusError {
	reason := rbac.ForbiddenMessage(a)
	if !a.ResourceRequest {
		return newStatus(http.StatusForbidden, metav1.ReasonForbidden, "forbidden: %s", reason)
	}
	resource := a.Resource
	if a.APIGroup != "" {
		resource += "." + a.APIGroup
	}
	var e *statusError
	if a.Name != "" {
		e = newStatus(http.StatusForbidden, metav1.ReasonForbidden, "%s %q is forbidden: %s", resource, a.Name, reason)
	} else {
		e = newStatus(http.StatusForbidden, metav1.ReasonForbidden, "%s is forbidden: %s", resource, reason)
	}
	e.status.Details = &metav1.StatusDetails{Name: a.Name, Kind: a.Resource}
	return e
}

// rbacStore reads the RBAC objects for the authorizer.
type rbacStore struct{ s *Server }

func listAs[T any](ctx context.Context, s *Server, res scheme.ResourceInfo, namespace string) ([]T, error) {
	objs, _, err := s.list(ctx, res, namespace)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(objs))
	for _, o := range objs {
		out = append(out, *any(o).(*T))
	}
	return out, nil
}

func (rs rbacStore) ClusterRoleBindings(ctx context.Context) ([]rbacv1.ClusterRoleBinding, error) {
	return listAs[rbacv1.ClusterRoleBinding](ctx, rs.s, scheme.ClusterRoleBindings, "")
}

func (rs rbacStore) RoleBindings(ctx context.Context, namespace string) ([]rbacv1.RoleBinding, error) {
	return listAs[rbacv1.RoleBinding](ctx, rs.s, scheme.RoleBindings, namespace)
}

func (rs rbacStore) ClusterRole(ctx context.Context, name string) (*rbacv1.ClusterRole, error) {
	obj, err := rs.s.get(ctx, scheme.ClusterRoles, "", name)
	if err != nil {
		return nil, ignoreNotFound(err)
	}
	return obj.(*rbacv1.ClusterRole), nil
}

func (rs rbacStore) Role(ctx context.Context, namespace, name string) (*rbacv1.Role, error) {
	obj, err := rs.s.get(ctx, scheme.Roles, namespace, name)
	if err != nil {
		return nil, ignoreNotFound(err)
	}
	return obj.(*rbacv1.Role), nil
}

func ignoreNotFound(err error) error {
	var se *statusError
	if errors.As(err, &se) && se.status.Reason == metav1.ReasonNotFound {
		return nil
	}
	return err
}

// bootstrapAuth loads (or creates) the service account signing key and
// creates the default ClusterRoles and bindings that are missing.
func (s *Server) bootstrapAuth(ctx context.Context) error {
	key := s.cfg.Prefix + "/serviceaccount-signing-key"
	kv, err := s.cfg.Store.Get(ctx, key)
	switch {
	case err == nil:
		s.saKey = kv.Value
	case errors.Is(err, etcd3.ErrNotFound):
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		if _, err := s.cfg.Store.Create(ctx, key, b, 0); err != nil {
			if !errors.Is(err, etcd3.ErrAlreadyExists) {
				return err
			}
			if kv, err = s.cfg.Store.Get(ctx, key); err != nil {
				return err
			}
			b = kv.Value
		}
		s.saKey = b
	default:
		return err
	}
	for _, cr := range bootstrappolicy.ClusterRoles() {
		if err := s.createIfMissing(ctx, scheme.ClusterRoles, &cr); err != nil {
			return err
		}
	}
	for _, crb := range bootstrappolicy.ClusterRoleBindings() {
		if err := s.createIfMissing(ctx, scheme.ClusterRoleBindings, &crb); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) createIfMissing(ctx context.Context, res scheme.ResourceInfo, obj metav1.Object) error {
	_, err := s.createFlow(ctx, requestInfo{res: res, namespace: obj.GetObjectMeta().Namespace}, obj, false)
	var se *statusError
	if err != nil && !(errors.As(err, &se) && se.status.Reason == metav1.ReasonAlreadyExists) {
		return fmt.Errorf("bootstrap %s %s: %w", res.Resource, obj.GetObjectMeta().Name, err)
	}
	return nil
}

// ensureDefaultServiceAccount creates the default ServiceAccount of a new
// namespace right away, so Pods can be created in it at once (the
// ServiceAccount controller recreates it if it is deleted).
func (s *Server) ensureDefaultServiceAccount(ctx context.Context, namespace string) {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: corev1.DefaultServiceAccountName, Namespace: namespace}}
	_ = s.createIfMissing(ctx, scheme.ServiceAccounts, sa)
}

// Token lifetimes of TokenRequest.
const (
	defaultTokenExpiration = time.Hour
	minTokenExpiration     = 10 * time.Minute
	maxTokenExpiration     = 48 * time.Hour
)

// handleTokenRequest issues a token for a service account
// (POST serviceaccounts/{name}/token), as the TokenRequest API.
func (s *Server) handleTokenRequest(w http.ResponseWriter, r *http.Request, info requestInfo) {
	ctx := r.Context()
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var req authenticationv1.TokenRequest
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := decodeStrict(body, &req); err != nil {
			writeError(w, errBadRequest("strict decoding error: %v", err))
			return
		}
	}
	saObj, err := s.get(ctx, scheme.ServiceAccounts, info.namespace, info.name)
	if err != nil {
		writeError(w, err)
		return
	}
	ttl := defaultTokenExpiration
	if e := req.Spec.ExpirationSeconds; e != nil {
		ttl = time.Duration(*e) * time.Second
	}
	if ttl < minTokenExpiration {
		writeError(w, errInvalid(scheme.ServiceAccounts, info.name, []string{fmt.Sprintf(
			"spec.expirationSeconds: Invalid value: %d: may not specify a duration less than 10 minutes", int64(ttl/time.Second))}))
		return
	}
	ttl = min(ttl, maxTokenExpiration)
	if len(req.Spec.Audiences) == 0 {
		req.Spec.Audiences = []string{serviceaccount.Issuer}
	}
	private := serviceaccount.PrivateClaims{Namespace: info.namespace,
		ServiceAccount: serviceaccount.Ref{Name: info.name, UID: saObj.GetObjectMeta().UID}}
	if ref := req.Spec.BoundObjectRef; ref != nil {
		if ref.Kind != "Pod" {
			writeError(w, errBadRequest("cannot bind token to object of kind %q; only Pods are supported", ref.Kind))
			return
		}
		podObj, err := s.get(ctx, scheme.Pods, info.namespace, ref.Name)
		if err != nil {
			writeError(w, err)
			return
		}
		pod := podObj.(*corev1.Pod)
		if ref.UID != "" && ref.UID != pod.UID {
			writeError(w, newStatus(http.StatusConflict, metav1.ReasonConflict, "the UID in the bound object reference (%s) does not match the UID in record. The object might have been deleted and then recreated", ref.UID))
			return
		}
		if sa := pod.Spec.ServiceAccountName; sa != info.name {
			writeError(w, errBadRequest("cannot bind token for serviceaccount %q to pod running with different serviceaccount name", info.name))
			return
		}
		private.Pod = &serviceaccount.Ref{Name: pod.Name, UID: pod.UID}
	}
	now := time.Now()
	token, err := serviceaccount.Sign(s.saKey, serviceaccount.NewClaims(now, ttl, req.Spec.Audiences, private))
	if err != nil {
		writeError(w, err)
		return
	}
	req.TypeMeta = metav1.TypeMeta{Kind: "TokenRequest", APIVersion: "authentication.k8s.io/v1"}
	req.ObjectMeta = metav1.ObjectMeta{Name: info.name, Namespace: info.namespace}
	req.Spec.ExpirationSeconds = ptrTo(int64(ttl / time.Second))
	req.Status = authenticationv1.TokenRequestStatus{Token: token, ExpirationTimestamp: metav1.Time{Time: now.Add(ttl).Truncate(time.Second)}}
	writeJSON(w, http.StatusCreated, req)
}

func ptrTo[T any](v T) *T { return &v }

// Review endpoints, served without storage.
const (
	selfSubjectAccessReviewsPath = "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"
	selfSubjectReviewsPath       = "/apis/authentication.k8s.io/v1/selfsubjectreviews"
)

// reviewResources are listed in discovery for the review groups.
var reviewResources = map[string]map[string]any{
	"authorization.k8s.io/v1": {"name": "selfsubjectaccessreviews", "singularName": "selfsubjectaccessreview",
		"namespaced": false, "kind": "SelfSubjectAccessReview", "verbs": []string{"create"}},
	"authentication.k8s.io/v1": {"name": "selfsubjectreviews", "singularName": "selfsubjectreview",
		"namespaced": false, "kind": "SelfSubjectReview", "verbs": []string{"create"}},
}

// handleReview answers SelfSubjectAccessReviews and SelfSubjectReviews.
func (s *Server) handleReview(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodPost {
		writeError(w, newStatus(http.StatusMethodNotAllowed, metav1.ReasonMethodNotAllowed, "%s is not supported on this path", r.Method))
		return
	}
	u := userFrom(r.Context())
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if path == selfSubjectReviewsPath {
		review := authenticationv1.SelfSubjectReview{TypeMeta: metav1.TypeMeta{Kind: "SelfSubjectReview", APIVersion: "authentication.k8s.io/v1"},
			Status: authenticationv1.SelfSubjectReviewStatus{UserInfo: authenticationv1.UserInfo{Username: u.Name, UID: u.UID, Groups: u.Groups}}}
		writeJSON(w, http.StatusCreated, review)
		return
	}
	var review authorizationv1.SelfSubjectAccessReview
	if err := json.Unmarshal(body, &review); err != nil {
		writeError(w, errBadRequest("invalid SelfSubjectAccessReview: %v", err))
		return
	}
	attrs := rbac.Attributes{User: u}
	switch ra, nra := review.Spec.ResourceAttributes, review.Spec.NonResourceAttributes; {
	case ra != nil:
		attrs.ResourceRequest, attrs.Verb, attrs.APIGroup, attrs.Resource = true, ra.Verb, ra.Group, ra.Resource
		attrs.Subresource, attrs.Namespace, attrs.Name = ra.Subresource, ra.Namespace, ra.Name
	case nra != nil:
		attrs.Verb, attrs.Path = nra.Verb, nra.Path
	default:
		writeError(w, errInvalid(scheme.ResourceInfo{Kind: "SelfSubjectAccessReview"}, "",
			[]string{"spec.resourceAttributes: Invalid value: \"null\": exactly one of nonResourceAttributes or resourceAttributes must be specified"}))
		return
	}
	allowed, reason, err := s.authorizer.Authorize(r.Context(), attrs)
	review.TypeMeta = metav1.TypeMeta{Kind: "SelfSubjectAccessReview", APIVersion: "authorization.k8s.io/v1"}
	review.Status = authorizationv1.SubjectAccessReviewStatus{Allowed: allowed, Reason: reason}
	if err != nil {
		review.Status.EvaluationError = err.Error()
	}
	writeJSON(w, http.StatusCreated, review)
}

// admitServiceAccount is the ServiceAccount admission plugin: a Pod runs
// as an existing service account ("default" unless named) and, unless
// either opts out, gets a projected volume with a token of it and the
// namespace, mounted in every container. Mirror Pods are left alone.
func (s *Server) admitServiceAccount(ctx context.Context, pod *corev1.Pod) error {
	if corev1.IsMirrorPod(pod) {
		return nil
	}
	name := pod.Spec.ServiceAccountName
	if name == "" {
		name = pod.Spec.ServiceAccount
	}
	if name == "" {
		name = corev1.DefaultServiceAccountName
	}
	pod.Spec.ServiceAccountName, pod.Spec.ServiceAccount = name, name
	obj, err := s.get(ctx, scheme.ServiceAccounts, pod.Namespace, name)
	if err != nil {
		if ignoreNotFound(err) == nil {
			return newStatus(http.StatusForbidden, metav1.ReasonForbidden,
				"pods %q is forbidden: error looking up service account %s/%s: serviceaccount %q not found", pod.Name, pod.Namespace, name, name)
		}
		return err
	}
	automount := true
	if a := obj.(*corev1.ServiceAccount).AutomountServiceAccountToken; a != nil {
		automount = *a
	}
	if a := pod.Spec.AutomountServiceAccountToken; a != nil {
		automount = *a
	}
	if !automount {
		return nil
	}
	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		for _, m := range c.VolumeMounts {
			if m.MountPath == corev1.ServiceAccountMountPath {
				return nil // the Pod brings its own
			}
		}
	}
	volume := corev1.ServiceAccountVolumePrefix + randomSuffix()
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: volume, VolumeSource: corev1.VolumeSource{
		Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptrTo[int32](0o644), Sources: []corev1.VolumeProjection{
			{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{ExpirationSeconds: ptrTo[int64](corev1.ServiceAccountTokenExpirationSeconds),
				Path: "token"}},
			{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace",
				FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
		}}}})
	mount := corev1.VolumeMount{Name: volume, MountPath: corev1.ServiceAccountMountPath, ReadOnly: true}
	for i := range pod.Spec.InitContainers {
		pod.Spec.InitContainers[i].VolumeMounts = append(pod.Spec.InitContainers[i].VolumeMounts, mount)
	}
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].VolumeMounts = append(pod.Spec.Containers[i].VolumeMounts, mount)
	}
	return nil
}
