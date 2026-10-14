// Package fields implements field selectors, mirroring
// k8s.io/apimachinery/pkg/fields.
package fields

import (
	"fmt"
	"strings"
)

// Selector is a conjunction of exact field matches.
type Selector []Requirement

// Requirement is "field=value" or "field!=value".
type Requirement struct {
	Field  string
	Value  string
	Negate bool
}

// ParseSelector parses "spec.nodeName=worker-1,status.phase!=Failed".
func ParseSelector(s string) (Selector, error) {
	var sel Selector
	for _, term := range strings.Split(s, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		negate := false
		var k, v string
		var ok bool
		if k, v, ok = strings.Cut(term, "!="); ok {
			negate = true
		} else if k, v, ok = strings.Cut(term, "=="); !ok {
			if k, v, ok = strings.Cut(term, "="); !ok {
				return nil, fmt.Errorf("invalid field selector term %q", term)
			}
		}
		sel = append(sel, Requirement{Field: strings.TrimSpace(k), Value: strings.TrimSpace(v), Negate: negate})
	}
	return sel, nil
}

// Matches evaluates the selector against a field set. Unknown fields fail.
func (s Selector) Matches(fields map[string]string) bool {
	for _, r := range s {
		v, ok := fields[r.Field]
		if !ok || (v == r.Value) == r.Negate {
			return false
		}
	}
	return true
}

// Validate rejects fields the resource does not support.
func (s Selector) Validate(supported map[string]string) error {
	for _, r := range s {
		if _, ok := supported[r.Field]; !ok {
			return fmt.Errorf("field selector %q is not supported", r.Field)
		}
	}
	return nil
}
