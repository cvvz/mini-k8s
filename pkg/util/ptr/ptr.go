// Package ptr has pointer helpers, mirroring k8s.io/utils/ptr.
package ptr

// To returns a pointer to v.
func To[T any](v T) *T { return &v }

// Deref returns *p, or def when p is nil.
func Deref[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
