package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/fields"
	"mini-k8s/pkg/labels"
	"mini-k8s/pkg/scheme"
)

const maxBody = 3 << 20

func readBody(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, errBadRequest("read body: %v", err)
	}
	if len(data) > maxBody {
		return nil, newStatus(http.StatusRequestEntityTooLarge, metav1.ReasonBadRequest, "request body too large")
	}
	return data, nil
}

// selectors parses and validates labelSelector/fieldSelector query parameters.
func (s *Server) selectors(info requestInfo, q map[string][]string) (labels.Selector, fields.Selector, error) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	sel, err := labels.Parse(get("labelSelector"))
	if err != nil {
		return nil, nil, errBadRequest("invalid labelSelector: %v", err)
	}
	fsel, err := fields.ParseSelector(get("fieldSelector"))
	if err != nil {
		return nil, nil, errBadRequest("invalid fieldSelector: %v", err)
	}
	if err := fsel.Validate(s.fieldsOf(info.res, info.res.New())); err != nil {
		return nil, nil, errBadRequest("%v", err)
	}
	return sel, fsel, nil
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, info requestInfo) {
	q := r.URL.Query()
	sel, fsel, err := s.selectors(info, q)
	if err != nil {
		writeError(w, err)
		return
	}
	if q.Get("watch") == "true" || q.Get("watch") == "1" {
		s.serveWatch(w, r, info, sel, fsel)
		return
	}
	objs, rev, err := s.list(r.Context(), info.res, info.namespace)
	if err != nil {
		writeError(w, err)
		return
	}
	list := metav1.List[metav1.Object]{
		TypeMeta: metav1.TypeMeta{APIVersion: info.res.GroupVersion(), Kind: info.res.Kind + "List"},
		ListMeta: metav1.ListMeta{ResourceVersion: strconv.FormatInt(rev, 10)},
		Items:    []metav1.Object{},
	}
	for _, obj := range objs {
		if s.matches(info.res, obj, sel, fsel) {
			list.Items = append(list.Items, obj)
		}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request, info requestInfo, dryRun bool) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	obj := info.res.New()
	if err := decodeStrict(body, obj); err != nil {
		writeError(w, errBadRequest("strict decoding error: %v", err))
		return
	}
	created, err := s.createFlow(r.Context(), info, obj, dryRun)
	respond(w, http.StatusCreated, created, err)
}

func checkTypeMeta(res scheme.ResourceInfo, obj metav1.Object) error {
	t := obj.GetTypeMeta()
	if (t.Kind != "" && t.Kind != res.Kind) || (t.APIVersion != "" && t.APIVersion != res.GroupVersion()) {
		return errBadRequest("%s/%s does not match the request path (%s %s)", t.APIVersion, t.Kind, res.GroupVersion(), res.Kind)
	}
	res.SetTypeMeta(obj)
	return nil
}

func (s *Server) createFlow(ctx context.Context, info requestInfo, obj metav1.Object, dryRun bool) (metav1.Object, error) {
	res := info.res
	if err := checkTypeMeta(res, obj); err != nil {
		return nil, err
	}
	m := obj.GetObjectMeta()
	if res.Namespaced {
		if m.Namespace == "" {
			m.Namespace = info.namespace
		} else if m.Namespace != info.namespace {
			return nil, errBadRequest("the namespace of the provided object (%s) does not match the namespace sent on the request (%s)", m.Namespace, info.namespace)
		}
	} else {
		m.Namespace = ""
	}
	if m.ResourceVersion != "" {
		return nil, errBadRequest("resourceVersion should not be set on objects to be created")
	}
	st := s.cfg.Strategies[res.Resource]
	generated := m.Name == "" && m.GenerateName != ""
	if generated {
		base := m.GenerateName
		if len(base) > 58 {
			base = base[:58]
		}
		m.Name = base + randomSuffix()
	}
	if m.Name == "" {
		return nil, errInvalid(res, "", []string{"metadata.name: Required value: name or generateName is required"})
	}
	if !validName(m.Name, st.NameIsDNSLabel) {
		return nil, errInvalid(res, m.Name, []string{"metadata.name: Invalid value: must be a lowercase RFC 1123 DNS name"})
	}
	if res.Namespaced {
		if err := s.admitNamespace(ctx, res, m.Namespace); err != nil {
			return nil, err
		}
	}
	m.UID = newUID()
	m.CreationTimestamp = metav1.Now()
	m.DeletionTimestamp, m.DeletionGracePeriodSeconds = nil, nil
	m.Generation = 0
	if _, ok := field(obj, "Spec"); ok {
		m.Generation = 1
	}
	if res.HasStatus {
		resetField(obj, "Status")
	}
	if st.Defaults != nil {
		st.Defaults(obj)
	}
	if st.PrepareForCreate != nil {
		st.PrepareForCreate(obj)
	}
	if errs := s.validate(res, st, obj); len(errs) > 0 {
		return nil, errInvalid(res, m.Name, errs)
	}
	if err := s.admitCreate(ctx, res, obj); err != nil {
		return nil, err
	}
	if dryRun {
		return obj, nil
	}
	for attempt := 0; ; attempt++ {
		created, err := s.create(ctx, res, obj)
		var se *statusError
		if generated && attempt < 5 && errors.As(err, &se) && se.status.Reason == metav1.ReasonAlreadyExists {
			m.Name = m.GenerateName + randomSuffix()
			continue
		}
		return created, err
	}
}

func (s *Server) validate(res scheme.ResourceInfo, st Strategy, obj metav1.Object) []string {
	m := obj.GetObjectMeta()
	var errs []string
	if err := labels.ValidateLabels(m.Labels); err != nil {
		errs = append(errs, "metadata.labels: Invalid value: "+err.Error())
	}
	for _, ref := range m.OwnerReferences {
		if ref.UID == "" || ref.Name == "" || ref.Kind == "" || ref.APIVersion == "" {
			errs = append(errs, "metadata.ownerReferences: Invalid value: apiVersion, kind, name and uid are required")
		}
	}
	if st.Validate != nil {
		errs = append(errs, st.Validate(obj)...)
	}
	return errs
}

// admitNamespace rejects creates in missing or terminating namespaces.
func (s *Server) admitNamespace(ctx context.Context, res scheme.ResourceInfo, namespace string) error {
	nsObj, err := s.get(ctx, scheme.Namespaces, "", namespace)
	if err != nil {
		return err
	}
	if nsObj.(*corev1.Namespace).Status.Phase == corev1.NamespaceTerminating && res.Resource != scheme.Events.Resource {
		return newStatus(http.StatusForbidden, metav1.ReasonForbidden,
			"unable to create new content in namespace %s because it is being terminated", namespace)
	}
	return nil
}

// admitCreate applies resource-specific admission rules.
func (s *Server) admitCreate(ctx context.Context, res scheme.ResourceInfo, obj metav1.Object) error {
	if res.Resource != scheme.Services.Resource {
		return nil
	}
	// The HTTP gateway serves exactly one Service until ClusterIP networking exists.
	existing, _, err := s.list(ctx, scheme.Services, "")
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		m := existing[0].GetObjectMeta()
		return newStatus(http.StatusConflict, metav1.ReasonConflict,
			"only one Service is supported; Service %s/%s already exists", m.Namespace, m.Name)
	}
	return nil
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request, info requestInfo, dryRun bool) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	obj := info.res.New()
	if err := decodeStrict(body, obj); err != nil {
		writeError(w, errBadRequest("strict decoding error: %v", err))
		return
	}
	// A PUT without resourceVersion is an unconditional update: retry lost
	// races against the latest stored version instead of failing.
	unconditional := obj.GetObjectMeta().ResourceVersion == ""
	var updated metav1.Object
	for attempt := 1; ; attempt++ {
		candidate := obj
		if unconditional {
			candidate = info.res.New()
			_ = json.Unmarshal(body, candidate)
		}
		updated, err = s.updateFlow(r.Context(), info, candidate, dryRun)
		if !unconditional || !isStaleWrite(err) || attempt == maxUnconditionalAttempts {
			break
		}
	}
	respond(w, http.StatusOK, updated, err)
}

// patchHasResourceVersion reports whether a merge patch pins metadata.resourceVersion.
func patchHasResourceVersion(patch []byte) bool {
	var p struct {
		Metadata struct {
			ResourceVersion *string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	return json.Unmarshal(patch, &p) == nil && p.Metadata.ResourceVersion != nil
}

func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request, info requestInfo, dryRun bool) {
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	switch ct {
	case "application/merge-patch+json", "application/strategic-merge-patch+json", "application/json":
	default:
		writeError(w, newStatus(http.StatusUnsupportedMediaType, metav1.ReasonBadRequest, "unsupported patch type %q; use application/merge-patch+json", ct))
		return
	}
	patch, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	// Like GuaranteedUpdate: a patch that does not pin a resourceVersion is
	// re-applied to fresh state when a concurrent writer wins the race.
	conditional := patchHasResourceVersion(patch)
	for attempt := 1; ; attempt++ {
		updated, err := s.patchOnce(r.Context(), info, patch, dryRun)
		if conditional || !isStaleWrite(err) || attempt == maxUnconditionalAttempts {
			respond(w, http.StatusOK, updated, err)
			return
		}
	}
}

func (s *Server) patchOnce(ctx context.Context, info requestInfo, patch []byte, dryRun bool) (metav1.Object, error) {
	old, err := s.get(ctx, info.res, info.namespace, info.name)
	if err != nil {
		return nil, err
	}
	oldJSON, err := json.Marshal(old)
	if err != nil {
		return nil, err
	}
	merged, err := mergePatch(oldJSON, patch)
	if err != nil {
		return nil, errBadRequest("invalid merge patch: %v", err)
	}
	obj := info.res.New()
	if err := decodeStrict(merged, obj); err != nil {
		return nil, errBadRequest("strict decoding error: %v", err)
	}
	return s.updateFlow(ctx, info, obj, dryRun)
}

func (s *Server) updateFlow(ctx context.Context, info requestInfo, obj metav1.Object, dryRun bool) (metav1.Object, error) {
	res := info.res
	if err := checkTypeMeta(res, obj); err != nil {
		return nil, err
	}
	nm := obj.GetObjectMeta()
	if nm.Name != "" && nm.Name != info.name {
		return nil, errBadRequest("the name of the object (%s) does not match the name on the URL (%s)", nm.Name, info.name)
	}
	if res.Namespaced && nm.Namespace != "" && nm.Namespace != info.namespace {
		return nil, errBadRequest("the namespace of the object (%s) does not match the namespace on the URL (%s)", nm.Namespace, info.namespace)
	}
	old, err := s.get(ctx, res, info.namespace, info.name)
	if err != nil {
		return nil, err
	}
	om := old.GetObjectMeta()
	if nm.UID != "" && nm.UID != om.UID {
		return nil, newStatus(http.StatusConflict, metav1.ReasonConflict, "Precondition failed: UID in object meta: %s, UID in storage: %s", nm.UID, om.UID)
	}
	if nm.ResourceVersion != "" && nm.ResourceVersion != om.ResourceVersion {
		return nil, errConflict(res, info.name)
	}
	var result metav1.Object
	if info.subresource == "status" {
		result = res.New()
		data, _ := json.Marshal(old)
		_ = json.Unmarshal(data, result)
		copyField(result, obj, "Status")
	} else {
		result = obj
		nm.Name, nm.Namespace, nm.UID, nm.GenerateName = om.Name, om.Namespace, om.UID, om.GenerateName
		nm.CreationTimestamp, nm.Generation = om.CreationTimestamp, om.Generation
		nm.DeletionTimestamp, nm.DeletionGracePeriodSeconds = om.DeletionTimestamp, om.DeletionGracePeriodSeconds
		if res.HasStatus {
			copyField(result, old, "Status")
		}
	}
	rm := result.GetObjectMeta()
	rm.ResourceVersion = om.ResourceVersion
	res.SetTypeMeta(result)
	st := s.cfg.Strategies[res.Resource]
	if st.Defaults != nil {
		st.Defaults(result)
	}
	if errs := s.validate(res, st, result); len(errs) > 0 {
		return nil, errInvalid(res, info.name, errs)
	}
	if info.subresource != "status" && st.ValidateUpdate != nil {
		if errs := st.ValidateUpdate(result, old); len(errs) > 0 {
			return nil, errInvalid(res, info.name, errs)
		}
	}
	if specChanged(result, old) {
		rm.Generation = om.Generation + 1
	}
	if sameStoredForm(result, old) || dryRun {
		if dryRun {
			return result, nil
		}
		return old, nil
	}
	// Removing the last finalizer from an object pending deletion completes it.
	if rm.DeletionTimestamp != nil && len(rm.Finalizers) == 0 && (rm.DeletionGracePeriodSeconds == nil || *rm.DeletionGracePeriodSeconds == 0) {
		return result, s.remove(ctx, res, result)
	}
	return s.update(ctx, res, result)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, info requestInfo, dryRun bool) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var opts metav1.DeleteOptions
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := decodeStrict(body, &opts); err != nil {
			writeError(w, errBadRequest("invalid DeleteOptions: %v", err))
			return
		}
	}
	q := r.URL.Query()
	if v := q.Get("gracePeriodSeconds"); v != "" {
		g, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, errBadRequest("invalid gracePeriodSeconds %q", v))
			return
		}
		opts.GracePeriodSeconds = &g
	}
	if v := q.Get("propagationPolicy"); v != "" {
		p := metav1.DeletionPropagation(v)
		opts.PropagationPolicy = &p
	}
	// Without a resourceVersion precondition, a delete racing with another
	// writer is retried on the fresh object.
	conditional := opts.Preconditions != nil && opts.Preconditions.ResourceVersion != nil
	for attempt := 1; ; attempt++ {
		result, err := s.deleteFlow(r.Context(), info, opts, dryRun)
		if conditional || !isStaleWrite(err) || attempt == maxUnconditionalAttempts {
			respond(w, http.StatusOK, result, err)
			return
		}
	}
}

func (s *Server) deleteFlow(ctx context.Context, info requestInfo, opts metav1.DeleteOptions, dryRun bool) (metav1.Object, error) {
	res := info.res
	obj, err := s.get(ctx, res, info.namespace, info.name)
	if err != nil {
		return nil, err
	}
	m := obj.GetObjectMeta()
	if p := opts.Preconditions; p != nil {
		if p.UID != nil && *p.UID != m.UID {
			return nil, newStatus(http.StatusConflict, metav1.ReasonConflict, "Precondition failed: UID in precondition: %s, UID in object meta: %s", *p.UID, m.UID)
		}
		if p.ResourceVersion != nil && *p.ResourceVersion != m.ResourceVersion {
			return nil, newStatus(http.StatusConflict, metav1.ReasonConflict, "Precondition failed: ResourceVersion in precondition: %s, ResourceVersion in object meta: %s", *p.ResourceVersion, m.ResourceVersion)
		}
	}
	policy := metav1.DeletePropagationBackground
	if opts.PropagationPolicy != nil {
		policy = *opts.PropagationPolicy
		if policy != metav1.DeletePropagationBackground && policy != metav1.DeletePropagationForeground && policy != metav1.DeletePropagationOrphan {
			return nil, errBadRequest("unsupported propagationPolicy %q", policy)
		}
	}
	grace := int64(0)
	if pod, ok := obj.(*corev1.Pod); ok {
		grace = 30
		if g := pod.Spec.TerminationGracePeriodSeconds; g != nil {
			grace = *g
		}
		if opts.GracePeriodSeconds != nil {
			grace = *opts.GracePeriodSeconds
		}
		// Nothing runs for unscheduled or finished Pods, so nothing needs to stop.
		if pod.Spec.NodeName == "" || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			grace = 0
		}
		if grace < 0 {
			grace = 0
		}
	}
	if res.Resource == scheme.Events.Resource {
		// Events never own anything and the garbage collector ignores them.
		policy = metav1.DeletePropagationBackground
	}
	if m.DeletionTimestamp == nil {
		switch policy {
		case metav1.DeletePropagationOrphan:
			if !m.HasFinalizer(metav1.FinalizerOrphanDependents) {
				m.Finalizers = append(m.Finalizers, metav1.FinalizerOrphanDependents)
			}
		case metav1.DeletePropagationForeground:
			if !m.HasFinalizer(metav1.FinalizerDeleteDependents) {
				m.Finalizers = append(m.Finalizers, metav1.FinalizerDeleteDependents)
			}
		}
		ts := metav1.NewTime(time.Now().Add(time.Duration(grace) * time.Second))
		m.DeletionTimestamp, m.DeletionGracePeriodSeconds = &ts, &grace
		if ns, ok := obj.(*corev1.Namespace); ok {
			ns.Status.Phase = corev1.NamespaceTerminating
		}
	} else if m.DeletionGracePeriodSeconds != nil && grace < *m.DeletionGracePeriodSeconds {
		// A shorter grace period (for example a force delete) replaces the pending one.
		ts := metav1.NewTime(time.Now().Add(time.Duration(grace) * time.Second))
		m.DeletionTimestamp, m.DeletionGracePeriodSeconds = &ts, &grace
	} else if len(m.Finalizers) > 0 || (m.DeletionGracePeriodSeconds != nil && *m.DeletionGracePeriodSeconds > 0) {
		return obj, nil
	}
	if dryRun {
		return obj, nil
	}
	if len(m.Finalizers) == 0 && (m.DeletionGracePeriodSeconds == nil || *m.DeletionGracePeriodSeconds == 0) {
		return obj, s.remove(ctx, res, obj)
	}
	return s.update(ctx, res, obj)
}
