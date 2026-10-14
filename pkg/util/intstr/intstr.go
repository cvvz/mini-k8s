// Package intstr holds IntOrString, a value that is either a number or a
// name, such as a probe port (8080 or "http"), as in
// k8s.io/apimachinery/pkg/util/intstr.
package intstr

import (
	"encoding/json"
	"strconv"
)

// IntOrString is an int32 or a string; exactly one form is used.
type IntOrString struct {
	IsString bool
	IntVal   int32
	StrVal   string
}

// FromInt returns an IntOrString holding n.
func FromInt(n int) IntOrString { return IntOrString{IntVal: int32(n)} }

// FromString returns an IntOrString holding s.
func FromString(s string) IntOrString { return IntOrString{IsString: true, StrVal: s} }

// String returns the value as text.
func (v IntOrString) String() string {
	if v.IsString {
		return v.StrVal
	}
	return strconv.Itoa(int(v.IntVal))
}

// MarshalJSON encodes a number or a string.
func (v IntOrString) MarshalJSON() ([]byte, error) {
	if v.IsString {
		return json.Marshal(v.StrVal)
	}
	return json.Marshal(v.IntVal)
}

// UnmarshalJSON decodes a number or a string.
func (v *IntOrString) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		v.IsString, v.IntVal = true, 0
		return json.Unmarshal(data, &v.StrVal)
	}
	v.IsString, v.StrVal = false, ""
	return json.Unmarshal(data, &v.IntVal)
}
