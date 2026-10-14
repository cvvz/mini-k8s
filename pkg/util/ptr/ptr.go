// Package ptr has pointer helpers, mirroring k8s.io/utils/ptr.
package ptr

// To returns a pointer to v.
func To[T any](v T) *T { return &v }
