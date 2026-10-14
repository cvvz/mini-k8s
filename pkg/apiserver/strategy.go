package apiserver

import (
	"context"
	"strings"

	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/fields"
	"mini-k8s/pkg/labels"
	"mini-k8s/pkg/scheme"
	"mini-k8s/pkg/util/validation"
)

// Strategy holds the resource-specific parts of request handling, like the
// create/update strategies of k8s.io/apiserver. Resources provide theirs in
// pkg/registry; nil members are skipped.
type Strategy struct {
	// Defaults fills omitted fields; it runs on create and on update.
	Defaults func(obj metav1.Object)
	// PrepareForCreate initialises server-owned state after status is reset.
	PrepareForCreate func(obj metav1.Object)
	// PrepareForUpdate carries server-owned values over from old before
	// validation (for example a Service's allocated cluster IP).
	PrepareForUpdate func(obj, old metav1.Object)
	// Validate returns "field: message" errors for a complete object.
	Validate func(obj metav1.Object) []string
	// ValidateUpdate returns errors for disallowed changes.
	ValidateUpdate func(obj, old metav1.Object) []string
	// NameIsDNSLabel requires names to be DNS labels (63 characters, no dots)
	// instead of DNS subdomains.
	NameIsDNSLabel bool
	// Fields returns selectable fields beyond metadata.name and metadata.namespace.
	Fields func(obj metav1.Object) map[string]string
	// BeginCreate and BeginUpdate run after validation and reserve what the
	// object needs, such as cluster IPs and node ports (like BeginCreate and
	// BeginUpdate of k8s.io/apiserver's generic registry). finish is called
	// with the outcome of the write so it can undo or complete the
	// reservation. A FieldErrors error is reported as Invalid.
	BeginCreate func(ctx context.Context, obj metav1.Object, dryRun bool) (finish FinishFunc, err error)
	BeginUpdate func(ctx context.Context, obj, old metav1.Object, dryRun bool) (finish FinishFunc, err error)
	// AfterDelete releases what a removed object held.
	AfterDelete func(ctx context.Context, obj metav1.Object)
}

// FinishFunc completes a Begin hook once the write succeeded or failed.
type FinishFunc func(ctx context.Context, success bool)

// FieldErrors are "field: message" validation errors found by a hook.
type FieldErrors []string

func (e FieldErrors) Error() string { return strings.Join(e, "; ") }

func validName(name string, label bool) bool {
	if label {
		return validation.IsDNS1123Label(name)
	}
	return validation.IsDNS1123Subdomain(name)
}

// fieldsOf returns the selectable fields of an object.
func (s *Server) fieldsOf(res scheme.ResourceInfo, obj metav1.Object) map[string]string {
	m := obj.GetObjectMeta()
	f := map[string]string{"metadata.name": m.Name, "metadata.namespace": m.Namespace}
	if st := s.cfg.Strategies[res.Resource]; st.Fields != nil {
		for k, v := range st.Fields(obj) {
			f[k] = v
		}
	}
	return f
}

func (s *Server) matches(res scheme.ResourceInfo, obj metav1.Object, sel labels.Selector, fsel fields.Selector) bool {
	return sel.Matches(obj.GetObjectMeta().Labels) && fsel.Matches(s.fieldsOf(res, obj))
}
