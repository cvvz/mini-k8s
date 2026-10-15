package v1

import (
	"encoding/json"
	"time"
)

// Duration is a time.Duration that marshals as a string like "1m0.5s", as
// metav1.Duration in Kubernetes.
type Duration struct {
	time.Duration
}

// MarshalJSON encodes the duration as Go formats it.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.Duration.String()) }

// UnmarshalJSON parses a Go duration string.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}
