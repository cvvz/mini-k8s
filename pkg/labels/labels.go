// Package labels implements label selectors, mirroring
// k8s.io/apimachinery/pkg/labels.
package labels

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Selector operators.
const (
	In           = "In"
	NotIn        = "NotIn"
	Exists       = "Exists"
	DoesNotExist = "DoesNotExist"
	opEquals     = "="
	opNotEquals  = "!="
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
	case In:
		return ok && contains(r.Values, v)
	case NotIn:
		return !ok || !contains(r.Values, v)
	case Exists:
		return ok
	case DoesNotExist:
		return !ok
	}
	return false
}

func (r Requirement) String() string {
	switch r.Operator {
	case opEquals, opNotEquals:
		return r.Key + r.Operator + r.Values[0]
	case In, NotIn:
		return r.Key + " " + strings.ToLower(r.Operator) + " (" + strings.Join(r.Values, ",") + ")"
	case Exists:
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
	case In, NotIn:
		if len(r.Values) == 0 {
			return fmt.Errorf("operator %s needs at least one value", r.Operator)
		}
	case Exists, DoesNotExist:
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

// Parse parses the string form used by `-l` and `labelSelector=`:
// "a=b,c!=d,e in (x,y),f notin (z),g,!h".
func Parse(s string) (Selector, error) {
	var sel Selector
	for _, term := range splitTerms(s) {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		var r Requirement
		switch {
		case strings.HasPrefix(term, "!"):
			r = Requirement{Key: strings.TrimSpace(term[1:]), Operator: DoesNotExist}
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
			op := map[string]string{"in": In, "notin": NotIn}[fields[1]]
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
			r = Requirement{Key: term, Operator: Exists}
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

// NewRequirement builds and validates one set-based requirement.
func NewRequirement(key, operator string, values []string) (Requirement, error) {
	r := Requirement{Key: key, Operator: operator, Values: values}
	return r, validateRequirement(r)
}
