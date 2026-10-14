// Package watch defines watch event types, mirroring
// k8s.io/apimachinery/pkg/watch.
package watch

// Event types sent on watch streams.
const (
	Added    = "ADDED"
	Modified = "MODIFIED"
	Deleted  = "DELETED"
	Error    = "ERROR"
)
