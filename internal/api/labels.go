package api

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

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

// Selector operators.
const (
	OpIn           = "In"
	OpNotIn        = "NotIn"
	OpExists       = "Exists"
	OpDoesNotExist = "DoesNotExist"
	opEquals       = "="
	opNotEquals    = "!="
)

// Requirement is one parsed selector term.
type Requirement struct {
	Key      string
	Operator string
	Values   []string
}

// Matches evaluates the requirement against a label set.
func (r Requirement) Matches(labels map[string]string) bool {
	v, ok := labels[r.Key]
	switch r.Operator {
	case opEquals:
		return ok && v == r.Values[0]
	case opNotEquals:
		return !ok || v != r.Values[0]
	case OpIn:
		return ok && contains(r.Values, v)
	case OpNotIn:
		return !ok || !contains(r.Values, v)
	case OpExists:
		return ok
	case OpDoesNotExist:
		return !ok
	}
	return false
}

func (r Requirement) String() string {
	switch r.Operator {
	case opEquals, opNotEquals:
		return r.Key + r.Operator + r.Values[0]
	case OpIn, OpNotIn:
		return r.Key + " " + strings.ToLower(r.Operator) + " (" + strings.Join(r.Values, ",") + ")"
	case OpExists:
		return r.Key
	default:
		return "!" + r.Key
	}
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// Selector is a conjunction of requirements; the empty selector matches everything.
type Selector []Requirement

// Matches reports whether all requirements hold.
func (s Selector) Matches(labels map[string]string) bool {
	for _, r := range s {
		if !r.Matches(labels) {
			return false
		}
	}
	return true
}

// Empty reports whether the selector matches everything.
func (s Selector) Empty() bool { return len(s) == 0 }

func (s Selector) String() string {
	parts := make([]string, len(s))
	for i, r := range s {
		parts[i] = r.String()
	}
	return strings.Join(parts, ",")
}

// SelectorFromSet builds an equality selector, sorted for stable output.
func SelectorFromSet(set map[string]string) Selector {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := make(Selector, 0, len(keys))
	for _, k := range keys {
		s = append(s, Requirement{Key: k, Operator: opEquals, Values: []string{set[k]}})
	}
	return s
}

// AsSelector converts the structured selector, validating operators.
func (ls *LabelSelector) AsSelector() (Selector, error) {
	if ls == nil {
		return nil, nil
	}
	s := SelectorFromSet(ls.MatchLabels)
	for _, e := range ls.MatchExpressions {
		r := Requirement{Key: e.Key, Operator: e.Operator, Values: e.Values}
		if err := validateRequirement(r); err != nil {
			return nil, err
		}
		s = append(s, r)
	}
	return s, nil
}

var labelKey = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
var labelValue = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)

// ValidateLabels checks keys and values against Kubernetes label syntax.
func ValidateLabels(labels map[string]string) error {
	for k, v := range labels {
		if !labelKey.MatchString(k) {
			return fmt.Errorf("invalid label key %q", k)
		}
		if !labelValue.MatchString(v) {
			return fmt.Errorf("invalid label value %q for key %q", v, k)
		}
	}
	return nil
}

func validateRequirement(r Requirement) error {
	if !labelKey.MatchString(r.Key) {
		return fmt.Errorf("invalid label key %q", r.Key)
	}
	switch r.Operator {
	case opEquals, opNotEquals:
		if len(r.Values) != 1 {
			return fmt.Errorf("operator %s needs exactly one value", r.Operator)
		}
	case OpIn, OpNotIn:
		if len(r.Values) == 0 {
			return fmt.Errorf("operator %s needs at least one value", r.Operator)
		}
	case OpExists, OpDoesNotExist:
		if len(r.Values) != 0 {
			return fmt.Errorf("operator %s takes no values", r.Operator)
		}
	default:
		return fmt.Errorf("unsupported selector operator %q", r.Operator)
	}
	for _, v := range r.Values {
		if !labelValue.MatchString(v) {
			return fmt.Errorf("invalid label value %q", v)
		}
	}
	return nil
}

// ParseSelector parses the string form used by `-l` and `labelSelector=`:
// "a=b,c!=d,e in (x,y),f notin (z),g,!h".
func ParseSelector(s string) (Selector, error) {
	var sel Selector
	for _, term := range splitTerms(s) {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		var r Requirement
		switch {
		case strings.HasPrefix(term, "!"):
			r = Requirement{Key: strings.TrimSpace(term[1:]), Operator: OpDoesNotExist}
		case strings.Contains(term, "!="):
			k, v, _ := strings.Cut(term, "!=")
			r = Requirement{Key: strings.TrimSpace(k), Operator: opNotEquals, Values: []string{strings.TrimSpace(v)}}
		case strings.Contains(term, "=="):
			k, v, _ := strings.Cut(term, "==")
			r = Requirement{Key: strings.TrimSpace(k), Operator: opEquals, Values: []string{strings.TrimSpace(v)}}
		case strings.Contains(term, "="):
			k, v, _ := strings.Cut(term, "=")
			r = Requirement{Key: strings.TrimSpace(k), Operator: opEquals, Values: []string{strings.TrimSpace(v)}}
		case strings.Contains(term, "("):
			fields := strings.Fields(term[:strings.Index(term, "(")])
			if len(fields) != 2 || !strings.HasSuffix(term, ")") {
				return nil, fmt.Errorf("invalid selector term %q", term)
			}
			op := map[string]string{"in": OpIn, "notin": OpNotIn}[fields[1]]
			if op == "" {
				return nil, fmt.Errorf("invalid selector operator %q", fields[1])
			}
			inner := term[strings.Index(term, "(")+1 : len(term)-1]
			var values []string
			for _, v := range strings.Split(inner, ",") {
				if v = strings.TrimSpace(v); v != "" {
					values = append(values, v)
				}
			}
			r = Requirement{Key: fields[0], Operator: op, Values: values}
		default:
			r = Requirement{Key: term, Operator: OpExists}
		}
		if err := validateRequirement(r); err != nil {
			return nil, err
		}
		sel = append(sel, r)
	}
	return sel, nil
}

// splitTerms splits on commas outside parentheses.
func splitTerms(s string) []string {
	var terms []string
	depth, start := 0, 0
	for i, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				terms = append(terms, s[start:i])
				start = i + 1
			}
		}
	}
	return append(terms, s[start:])
}

// FieldSelector is a conjunction of exact field matches.
type FieldSelector []FieldRequirement

// FieldRequirement is "field=value" or "field!=value".
type FieldRequirement struct {
	Field  string
	Value  string
	Negate bool
}

// ParseFieldSelector parses "spec.nodeName=worker-1,status.phase!=Failed".
func ParseFieldSelector(s string) (FieldSelector, error) {
	var sel FieldSelector
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
		sel = append(sel, FieldRequirement{Field: strings.TrimSpace(k), Value: strings.TrimSpace(v), Negate: negate})
	}
	return sel, nil
}

// Matches evaluates the selector against a field set. Unknown fields fail.
func (s FieldSelector) Matches(fields map[string]string) bool {
	for _, r := range s {
		v, ok := fields[r.Field]
		if !ok || (v == r.Value) == r.Negate {
			return false
		}
	}
	return true
}

// Validate rejects fields the resource does not support.
func (s FieldSelector) Validate(supported map[string]string) error {
	for _, r := range s {
		if _, ok := supported[r.Field]; !ok {
			return fmt.Errorf("field selector %q is not supported", r.Field)
		}
	}
	return nil
}
