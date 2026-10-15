// Package equality compares API values by what they serialize to, like
// k8s.io/apimachinery/pkg/api/equality.Semantic: a nil map or slice equals
// an empty one, which reflect.DeepEqual does not accept.
package equality

import (
	"bytes"
	"encoding/json"
)

// Semantic reports whether a and b encode to the same JSON.
func Semantic(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
}
