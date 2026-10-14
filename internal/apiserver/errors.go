package apiserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"mini-k8s/internal/api"
	"mini-k8s/internal/storage"
)

// statusError is an API error rendered as a Status object.
type statusError struct {
	status api.Status
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

func newStatus(code int, reason api.StatusReason, format string, args ...any) *statusError {
	return &statusError{status: api.Status{
		TypeMeta: api.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   api.StatusFailure, Code: int32(code), Reason: reason,
		Message: fmt.Sprintf(format, args...),
	}}
}

func errNotFound(res api.ResourceInfo, name string) *statusError {
	e := newStatus(http.StatusNotFound, api.ReasonNotFound, "%s %q not found", res.GroupResource(), name)
	e.status.Details = &api.StatusDetails{Name: name, Kind: res.Resource}
	return e
}

func errAlreadyExists(res api.ResourceInfo, name string) *statusError {
	e := newStatus(http.StatusConflict, api.ReasonAlreadyExists, "%s %q already exists", res.GroupResource(), name)
	e.status.Details = &api.StatusDetails{Name: name, Kind: res.Resource}
	return e
}

func errConflict(res api.ResourceInfo, name string) *statusError {
	e := newStatus(http.StatusConflict, api.ReasonConflict,
		"Operation cannot be fulfilled on %s %q: the object has been modified; please apply your changes to the latest version and try again",
		res.GroupResource(), name)
	e.staleWrite = true
	return e
}

// maxUnconditionalAttempts bounds server-side retries of writes that did not
// ask for a specific resourceVersion.
const maxUnconditionalAttempts = 8

func errBadRequest(format string, args ...any) *statusError {
	return newStatus(http.StatusBadRequest, api.ReasonBadRequest, format, args...)
}

// errInvalid reports field errors such as "spec.replicas: must be >= 0".
func errInvalid(res api.ResourceInfo, name string, causes []string) *statusError {
	e := newStatus(http.StatusUnprocessableEntity, api.ReasonInvalid, "%s %q is invalid: %s", res.Kind, name, strings.Join(causes, "; "))
	details := &api.StatusDetails{Name: name, Kind: res.Kind}
	for _, c := range causes {
		field, msg, _ := strings.Cut(c, ": ")
		details.Causes = append(details.Causes, api.StatusCause{Field: field, Message: msg})
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
	case errors.Is(err, storage.ErrNotFound):
		se = newStatus(http.StatusNotFound, api.ReasonNotFound, "%v", err)
	case errors.Is(err, storage.ErrConflict):
		se = newStatus(http.StatusConflict, api.ReasonConflict, "%v", err)
	default:
		se = newStatus(http.StatusInternalServerError, api.ReasonInternalError, "%v", err)
	}
	writeJSON(w, int(se.status.Code), se.status)
}
