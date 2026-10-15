// Package v1 holds the object metadata shared by every resource (metav1),
// mirroring k8s.io/apimachinery/pkg/apis/meta/v1 for the supported subset.
package v1

import (
	"encoding/json"
	"fmt"
	"time"

	"mini-k8s/pkg/labels"
)

// Object is implemented by every top-level resource through its embedded
// TypeMeta and ObjectMeta.
type Object interface {
	GetObjectMeta() *ObjectMeta
	GetTypeMeta() *TypeMeta
}

// TypeMeta identifies the schema of an object.
type TypeMeta struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
}

// GetTypeMeta implements Object.
func (t *TypeMeta) GetTypeMeta() *TypeMeta { return t }

// ObjectMeta is the metadata every persisted object carries.
type ObjectMeta struct {
	Name                       string            `json:"name,omitempty"`
	GenerateName               string            `json:"generateName,omitempty"`
	Namespace                  string            `json:"namespace,omitempty"`
	UID                        string            `json:"uid,omitempty"`
	ResourceVersion            string            `json:"resourceVersion,omitempty"`
	Generation                 int64             `json:"generation,omitempty"`
	CreationTimestamp          Time              `json:"creationTimestamp,omitzero"`
	DeletionTimestamp          *Time             `json:"deletionTimestamp,omitempty"`
	DeletionGracePeriodSeconds *int64            `json:"deletionGracePeriodSeconds,omitempty"`
	Labels                     map[string]string `json:"labels,omitempty"`
	Annotations                map[string]string `json:"annotations,omitempty"`
	OwnerReferences            []OwnerReference  `json:"ownerReferences,omitempty"`
	Finalizers                 []string          `json:"finalizers,omitempty"`
}

// GetObjectMeta implements Object.
func (m *ObjectMeta) GetObjectMeta() *ObjectMeta { return m }

// HasFinalizer reports whether name is present in Finalizers.
func (m *ObjectMeta) HasFinalizer(name string) bool {
	for _, f := range m.Finalizers {
		if f == name {
			return true
		}
	}
	return false
}

// RemoveFinalizer drops name from Finalizers and reports whether it was present.
func (m *ObjectMeta) RemoveFinalizer(name string) bool {
	kept := m.Finalizers[:0:0]
	for _, f := range m.Finalizers {
		if f != name {
			kept = append(kept, f)
		}
	}
	removed := len(kept) != len(m.Finalizers)
	m.Finalizers = kept
	if len(m.Finalizers) == 0 {
		m.Finalizers = nil
	}
	return removed
}

// ListMeta describes a list response.
type ListMeta struct {
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// List is the wire form of every collection, e.g. kind PodList.
type List[T any] struct {
	TypeMeta `json:",inline"`
	ListMeta ListMeta `json:"metadata"`
	Items    []T      `json:"items"`
}

// OwnerReference links a dependent to the object that owns it.
type OwnerReference struct {
	APIVersion         string `json:"apiVersion"`
	Kind               string `json:"kind"`
	Name               string `json:"name"`
	UID                string `json:"uid"`
	Controller         *bool  `json:"controller,omitempty"`
	BlockOwnerDeletion *bool  `json:"blockOwnerDeletion,omitempty"`
}

// NewControllerRef returns an owner reference marking owner as the managing controller.
func NewControllerRef(owner Object) OwnerReference {
	yes := true
	t, m := owner.GetTypeMeta(), owner.GetObjectMeta()
	return OwnerReference{APIVersion: t.APIVersion, Kind: t.Kind, Name: m.Name, UID: m.UID, Controller: &yes, BlockOwnerDeletion: &yes}
}

// ControllerOf returns the managing controller reference, if any.
func ControllerOf(m *ObjectMeta) *OwnerReference {
	for i := range m.OwnerReferences {
		if r := &m.OwnerReferences[i]; r.Controller != nil && *r.Controller {
			return r
		}
	}
	return nil
}

// PartialObjectMetadata decodes only the identity and metadata of any object.
type PartialObjectMetadata struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
}

// Time serialises as RFC 3339 with second precision, like metav1.Time; the
// zero value encodes as null.
type Time struct{ time.Time }

// Now returns the current time truncated to seconds.
func Now() Time { return NewTime(time.Now()) }

// NewTime wraps t in UTC with second precision.
func NewTime(t time.Time) Time { return Time{t.UTC().Truncate(time.Second)} }

// MarshalJSON implements json.Marshaler.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.UTC().Format(time.RFC3339))
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *Time) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		t.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("invalid time %q: %w", s, err)
	}
	t.Time = parsed.UTC()
	return nil
}

// MicroTime is Time with microsecond precision, as used by Leases.
type MicroTime struct{ time.Time }

// NowMicro returns the current time truncated to microseconds.
func NowMicro() MicroTime { return MicroTime{time.Now().UTC().Truncate(time.Microsecond)} }

// MarshalJSON implements json.Marshaler.
func (t MicroTime) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.UTC().Format("2006-01-02T15:04:05.000000Z07:00"))
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *MicroTime) UnmarshalJSON(b []byte) error {
	var parsed Time
	if err := parsed.UnmarshalJSON(b); err != nil {
		return err
	}
	t.Time = parsed.Time
	return nil
}

// StatusReason is a machine-readable error category.
type StatusReason string

// Status reasons used by the API server.
const (
	ReasonBadRequest       StatusReason = "BadRequest"
	ReasonUnauthorized     StatusReason = "Unauthorized"
	ReasonForbidden        StatusReason = "Forbidden"
	ReasonNotFound         StatusReason = "NotFound"
	ReasonAlreadyExists    StatusReason = "AlreadyExists"
	ReasonConflict         StatusReason = "Conflict"
	ReasonInvalid          StatusReason = "Invalid"
	ReasonMethodNotAllowed StatusReason = "MethodNotAllowed"
	ReasonExpired          StatusReason = "Expired"
	ReasonInternalError    StatusReason = "InternalError"
	ReasonServiceUnavail   StatusReason = "ServiceUnavailable"
)

// Status is returned for errors and for some non-object responses.
type Status struct {
	TypeMeta `json:",inline"`
	Metadata ListMeta       `json:"metadata"`
	Status   string         `json:"status,omitempty"`
	Message  string         `json:"message,omitempty"`
	Reason   StatusReason   `json:"reason,omitempty"`
	Details  *StatusDetails `json:"details,omitempty"`
	Code     int32          `json:"code,omitempty"`
}

// StatusDetails identifies the object an error refers to.
type StatusDetails struct {
	Name   string        `json:"name,omitempty"`
	Kind   string        `json:"kind,omitempty"`
	Causes []StatusCause `json:"causes,omitempty"`
}

// StatusCause describes one invalid field.
type StatusCause struct {
	Field   string `json:"field,omitempty"`
	Message string `json:"message,omitempty"`
}

// Status values.
const (
	StatusSuccess = "Success"
	StatusFailure = "Failure"
)

// DeletionPropagation selects how dependents are handled.
type DeletionPropagation string

// Propagation policies.
const (
	DeletePropagationOrphan     DeletionPropagation = "Orphan"
	DeletePropagationBackground DeletionPropagation = "Background"
	DeletePropagationForeground DeletionPropagation = "Foreground"
)

// Finalizers managed by the garbage collector.
const (
	FinalizerOrphanDependents = "orphan"
	FinalizerDeleteDependents = "foregroundDeletion"
)

// DeleteOptions accompanies DELETE requests.
type DeleteOptions struct {
	TypeMeta           `json:",inline"`
	GracePeriodSeconds *int64               `json:"gracePeriodSeconds,omitempty"`
	Preconditions      *Preconditions       `json:"preconditions,omitempty"`
	PropagationPolicy  *DeletionPropagation `json:"propagationPolicy,omitempty"`
	DryRun             []string             `json:"dryRun,omitempty"`
}

// Preconditions must hold for a delete to proceed.
type Preconditions struct {
	UID             *string `json:"uid,omitempty"`
	ResourceVersion *string `json:"resourceVersion,omitempty"`
}

// WatchEvent is one line of a watch stream.
type WatchEvent struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

// LabelSelector is the structured selector used in object specs.
type LabelSelector struct {
	MatchLabels      map[string]string          `json:"matchLabels,omitempty"`
	MatchExpressions []LabelSelectorRequirement `json:"matchExpressions,omitempty"`
}

// LabelSelectorRequirement is one set-based expression.
type LabelSelectorRequirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values,omitempty"`
}

// AsSelector converts the structured selector, validating operators.
func (ls *LabelSelector) AsSelector() (labels.Selector, error) {
	if ls == nil {
		return nil, nil
	}
	s := labels.SelectorFromSet(ls.MatchLabels)
	for _, e := range ls.MatchExpressions {
		r, err := labels.NewRequirement(e.Key, e.Operator, e.Values)
		if err != nil {
			return nil, err
		}
		s = append(s, r)
	}
	return s, nil
}
