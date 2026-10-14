package apiserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"mini-k8s/pkg/storage/etcd3"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/scheme"
)

// statusError is an API error rendered as a Status object.
type statusError struct {
	status metav1.Status
	// staleWrite marks a lost compare-and-swap race (not a failed client
	// precondition); unconditional writes retry these on fresh state.
	staleWrite bool
}

// isStaleWrite reports whether err is a retryable lost CAS race.
func isStaleWrite(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.staleWrite
}

func (e *statusError) Error() string { return e.status.Message }

func newStatus(code int, reason metav1.StatusReason, format string, args ...any) *statusError {
	return &statusError{status: metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure, Code: int32(code), Reason: reason,
		Message: fmt.Sprintf(format, args...),
	}}
}

func errNotFound(res scheme.ResourceInfo, name string) *statusError {
	e := newStatus(http.StatusNotFound, metav1.ReasonNotFound, "%s %q not found", res.GroupResource(), name)
	e.status.Details = &metav1.StatusDetails{Name: name, Kind: res.Resource}
	return e
}

func errAlreadyExists(res scheme.ResourceInfo, name string) *statusError {
	e := newStatus(http.StatusConflict, metav1.ReasonAlreadyExists, "%s %q already exists", res.GroupResource(), name)
	e.status.Details = &metav1.StatusDetails{Name: name, Kind: res.Resource}
	return e
}

func errConflict(res scheme.ResourceInfo, name string) *statusError {
	e := newStatus(http.StatusConflict, metav1.ReasonConflict,
		"Operation cannot be fulfilled on %s %q: the object has been modified; please apply your changes to the latest version and try again",
		res.GroupResource(), name)
	e.staleWrite = true
	return e
}

// maxUnconditionalAttempts bounds server-side retries of writes that did not
// ask for a specific resourceVersion.
const maxUnconditionalAttempts = 8

func errBadRequest(format string, args ...any) *statusError {
	return newStatus(http.StatusBadRequest, metav1.ReasonBadRequest, format, args...)
}

// errInvalid reports field errors such as "spec.replicas: must be >= 0".
func errInvalid(res scheme.ResourceInfo, name string, causes []string) *statusError {
	e := newStatus(http.StatusUnprocessableEntity, metav1.ReasonInvalid, "%s %q is invalid: %s", res.Kind, name, strings.Join(causes, "; "))
	details := &metav1.StatusDetails{Name: name, Kind: res.Kind}
	for _, c := range causes {
		field, msg, _ := strings.Cut(c, ": ")
		details.Causes = append(details.Causes, metav1.StatusCause{Field: field, Message: msg})
	}
	e.status.Details = details
	return e
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError renders any error as a Status, mapping storage errors.
func writeError(w http.ResponseWriter, err error) {
	var se *statusError
	switch {
	case errors.As(err, &se):
	case errors.Is(err, etcd3.ErrNotFound):
		se = newStatus(http.StatusNotFound, metav1.ReasonNotFound, "%v", err)
	case errors.Is(err, etcd3.ErrConflict):
		se = newStatus(http.StatusConflict, metav1.ReasonConflict, "%v", err)
	default:
		se = newStatus(http.StatusInternalServerError, metav1.ReasonInternalError, "%v", err)
	}
	writeJSON(w, int(se.status.Code), se.status)
}
