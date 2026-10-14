// Package apiserver implements the mini-k8s API server: Kubernetes-style
// REST paths, strict decoding, defaults and validation, optimistic
// concurrency, graceful deletion with finalizers, watch streams and the Pod
// binding/log subresources. etcd is the only state.
package apiserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/scheme"
	"mini-k8s/pkg/storage/etcd3"
)

// Config configures a Server.
type Config struct {
	Store *etcd3.Store
	// Token is the shared bearer token every request must present.
	Token string
	// Prefix is the etcd key prefix; tests use unique prefixes.
	Prefix string
	// KubeletPort is where kubelets serve logs.
	KubeletPort int
	// EventTTL bounds how long Events are kept after their last update.
	EventTTL time.Duration
	// WatchTimeout closes idle watch streams so clients re-establish them.
	WatchTimeout time.Duration
	// Strategies holds the per-resource strategies, keyed by resource
	// (see pkg/registry). Required.
	Strategies map[string]Strategy
}

// Server is an http.Handler serving the API.
type Server struct {
	cfg     Config
	kubelet *http.Client

	leaseMu      sync.Mutex
	eventLease   int64
	eventLeaseAt time.Time
}

// Version is reported by /version.
const Version = "v2.0.0-mini"

// New returns a Server with defaults applied to cfg.
func New(cfg Config) *Server {
	if cfg.Strategies == nil {
		panic("apiserver: Config.Strategies is required (use registry.Strategies())")
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "/registry"
	}
	if cfg.KubeletPort == 0 {
		cfg.KubeletPort = 10250
	}
	if cfg.EventTTL == 0 {
		cfg.EventTTL = time.Hour
	}
	if cfg.WatchTimeout == 0 {
		cfg.WatchTimeout = 5 * time.Minute
	}
	return &Server{cfg: cfg, kubelet: &http.Client{}}
}

// Bootstrap creates the namespaces every cluster has.
func (s *Server) Bootstrap(ctx context.Context) error {
	for _, name := range []string{corev1.NamespaceDefault, corev1.NamespaceSystem} {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if _, err := s.createFlow(ctx, requestInfo{res: scheme.Namespaces}, ns, false); err != nil {
			var se *statusError
			if !errors.As(err, &se) || se.status.Reason != metav1.ReasonAlreadyExists {
				return fmt.Errorf("bootstrap namespace %s: %w", name, err)
			}
		}
	}
	return nil
}

type requestInfo struct {
	res         scheme.ResourceInfo
	namespace   string
	name        string
	subresource string
	// allNamespaces is set for namespaced resources addressed without a namespace.
	allNamespaces bool
}

func (s *Server) authenticate(r *http.Request) bool {
	got := r.Header.Get("Authorization")
	return s.cfg.Token != "" && subtle.ConstantTimeCompare([]byte(got), []byte("Bearer "+s.cfg.Token)) == 1
}

// ServeHTTP routes a request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authenticate(r) {
		writeError(w, newStatus(http.StatusUnauthorized, metav1.ReasonUnauthorized, "Unauthorized"))
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch path {
	case "/healthz", "/readyz", "/livez":
		if err := s.cfg.Store.Health(r.Context()); err != nil {
			writeError(w, newStatus(http.StatusServiceUnavailable, metav1.ReasonServiceUnavail, "etcd unavailable: %v", err))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
		return
	case "/version":
		writeJSON(w, http.StatusOK, map[string]string{"gitVersion": Version, "platform": "linux"})
		return
	case "/api":
		writeJSON(w, http.StatusOK, map[string]any{"kind": "APIVersions", "versions": []string{"v1"}})
		return
	case "/apis":
		writeJSON(w, http.StatusOK, apiGroupList())
		return
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	var group, version string
	var rest []string
	switch {
	case len(segs) >= 2 && segs[0] == "api":
		version, rest = segs[1], segs[2:]
	case len(segs) >= 3 && segs[0] == "apis":
		group, version, rest = segs[1], segs[2], segs[3:]
	default:
		writeError(w, newStatus(http.StatusNotFound, metav1.ReasonNotFound, "the server could not find the requested resource"))
		return
	}
	if len(rest) == 0 {
		if list, ok := resourceList(group, version); ok && r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, list)
			return
		}
		writeError(w, newStatus(http.StatusNotFound, metav1.ReasonNotFound, "the server could not find the requested resource"))
		return
	}
	info, err := parseResourcePath(group, version, rest)
	if err != nil {
		writeError(w, err)
		return
	}
	s.dispatch(w, r, info)
}

func parseResourcePath(group, version string, rest []string) (requestInfo, error) {
	notFound := newStatus(http.StatusNotFound, metav1.ReasonNotFound, "the server could not find the requested resource")
	var info requestInfo
	namespaced := false
	if rest[0] == "namespaces" && len(rest) >= 3 {
		if res, ok := scheme.ForGroupVersionResource(group, version, rest[2]); ok && res.Namespaced {
			info.namespace, rest, namespaced = rest[1], rest[2:], true
		}
	}
	res, ok := scheme.ForGroupVersionResource(group, version, rest[0])
	if !ok || len(rest) > 3 {
		return info, notFound
	}
	info.res = res
	if len(rest) >= 2 {
		info.name = rest[1]
	}
	if len(rest) == 3 {
		info.subresource = rest[2]
	}
	if res.Namespaced && !namespaced {
		if info.name != "" {
			return info, notFound
		}
		info.allNamespaces = true
	}
	if namespaced && !validName(info.namespace, true) {
		return info, errBadRequest("invalid namespace %q", info.namespace)
	}
	if info.name != "" && !validName(info.name, false) {
		return info, errBadRequest("invalid name %q", info.name)
	}
	return info, nil
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, info requestInfo) {
	q := r.URL.Query()
	dryRun := q.Get("dryRun") == "All"
	sub := info.subresource
	if sub != "" && sub != "status" && !(info.res.Resource == scheme.Pods.Resource && (sub == "binding" || sub == "log")) {
		writeError(w, newStatus(http.StatusNotFound, metav1.ReasonNotFound, "%s has no subresource %q", info.res.Resource, sub))
		return
	}
	if sub == "status" && !info.res.HasStatus {
		writeError(w, newStatus(http.StatusNotFound, metav1.ReasonNotFound, "%s has no status subresource", info.res.Resource))
		return
	}
	if info.allNamespaces && r.Method != http.MethodGet {
		writeError(w, errBadRequest("a namespace is required for %s on %s", r.Method, info.res.Resource))
		return
	}
	ctx := r.Context()
	switch {
	case r.Method == http.MethodGet && info.name == "":
		s.handleList(w, r, info)
	case r.Method == http.MethodGet && sub == "log":
		s.handleLog(w, r, info)
	case r.Method == http.MethodGet:
		obj, err := s.get(ctx, info.res, info.namespace, info.name)
		respond(w, http.StatusOK, obj, err)
	case r.Method == http.MethodPost && info.name == "":
		s.handleCreate(w, r, info, dryRun)
	case r.Method == http.MethodPost && sub == "binding":
		s.handleBinding(w, r, info)
	case r.Method == http.MethodPut && info.name != "" && sub != "binding" && sub != "log":
		s.handleUpdate(w, r, info, dryRun)
	case r.Method == http.MethodPatch && info.name != "" && sub != "binding" && sub != "log":
		s.handlePatch(w, r, info, dryRun)
	case r.Method == http.MethodDelete && info.name != "" && sub == "":
		s.handleDelete(w, r, info, dryRun)
	default:
		writeError(w, newStatus(http.StatusMethodNotAllowed, metav1.ReasonMethodNotAllowed, "%s is not supported on this path", r.Method))
	}
}

func respond(w http.ResponseWriter, code int, obj any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, code, obj)
}

func newUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

const nameSuffixAlphabet = "bcdfghjklmnpqrstvwxz2456789"

func randomSuffix() string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = nameSuffixAlphabet[int(b[i])%len(nameSuffixAlphabet)]
	}
	return string(b[:])
}

func apiGroupList() map[string]any {
	groups := map[string]bool{}
	var list []any
	for _, r := range scheme.All {
		if r.Group == "" || groups[r.Group] {
			continue
		}
		groups[r.Group] = true
		gv := map[string]string{"groupVersion": r.GroupVersion(), "version": r.Version}
		list = append(list, map[string]any{"name": r.Group, "versions": []any{gv}, "preferredVersion": gv})
	}
	return map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": list}
}

func resourceList(group, version string) (map[string]any, bool) {
	var resources []any
	gv := ""
	for _, r := range scheme.All {
		if r.Group != group || r.Version != version {
			continue
		}
		gv = r.GroupVersion()
		verbs := []string{"create", "delete", "get", "list", "patch", "update", "watch"}
		resources = append(resources, map[string]any{
			"name": r.Resource, "singularName": r.Singular, "namespaced": r.Namespaced,
			"kind": r.Kind, "verbs": verbs, "shortNames": r.ShortNames,
		})
		if r.HasStatus {
			resources = append(resources, map[string]any{"name": r.Resource + "/status", "namespaced": r.Namespaced, "kind": r.Kind, "verbs": []string{"get", "patch", "update"}})
		}
		if r.Resource == scheme.Pods.Resource {
			resources = append(resources,
				map[string]any{"name": "pods/binding", "namespaced": true, "kind": "Binding", "verbs": []string{"create"}},
				map[string]any{"name": "pods/log", "namespaced": true, "kind": "Pod", "verbs": []string{"get"}})
		}
	}
	if gv == "" {
		return nil, false
	}
	return map[string]any{"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": gv, "resources": resources}, true
}
