// Package controller holds helpers shared by the controller-manager's
// controllers, like k8s.io/kubernetes/pkg/controller/controller_utils.go.
// Each controller lives in its own subpackage.
package controller

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

// Expectations stop a controller from acting on a stale cache: after it
// creates or deletes Pods it waits until the informer has observed those
// changes (or a timeout passes) before computing a new diff.
type Expectations struct {
	mu sync.Mutex
	m  map[string]*expectation
}

type expectation struct {
	adds int
	dels map[string]bool
	at   time.Time
}

const expectationTimeout = time.Minute

// NewExpectations returns an empty expectation tracker.
func NewExpectations() *Expectations { return &Expectations{m: map[string]*expectation{}} }

// Expect records pending creations and the UIDs of pending deletions.
func (e *Expectations) Expect(key string, adds int, dels []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	x := &expectation{adds: adds, dels: map[string]bool{}, at: time.Now()}
	for _, uid := range dels {
		x.dels[uid] = true
	}
	e.m[key] = x
}

// Raise adds pending creations and deletions to those still expected, for
// controllers that create and delete several objects concurrently.
func (e *Expectations) Raise(key string, adds int, dels []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	x := e.m[key]
	if x == nil || time.Since(x.at) > expectationTimeout {
		x = &expectation{dels: map[string]bool{}}
		e.m[key] = x
	}
	x.adds, x.at = max(x.adds, 0)+adds, time.Now()
	for _, uid := range dels {
		x.dels[uid] = true
	}
}

// ObserveAdd records that one expected creation was observed.
func (e *Expectations) ObserveAdd(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if x := e.m[key]; x != nil {
		x.adds--
	}
}

// ObserveDelete records that the deletion of uid was observed.
func (e *Expectations) ObserveDelete(key, uid string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if x := e.m[key]; x != nil {
		delete(x.dels, uid)
	}
}

// Satisfied reports whether key may act on the cache: nothing is pending
// or the expectations have expired.
func (e *Expectations) Satisfied(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	x := e.m[key]
	return x == nil || (x.adds <= 0 && len(x.dels) == 0) || time.Since(x.at) > expectationTimeout
}

// Forget drops the expectations of key.
func (e *Expectations) Forget(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.m, key)
}

// OwnerKey returns "namespace/name" of the managing controller of the given
// kind, or "" when obj has no such controller.
func OwnerKey(obj metav1.Object, kind string) string {
	m := obj.GetObjectMeta()
	if ref := metav1.ControllerOf(m); ref != nil && ref.Kind == kind {
		return m.Namespace + "/" + ref.Name
	}
	return ""
}

// ComputeHash identifies a Pod template, like controller.ComputeHash in
// Kubernetes: the FNV-32a hash of the template, spelled safely. It names
// a Deployment's ReplicaSets and a DaemonSet's ControllerRevisions.
func ComputeHash(tpl *corev1.PodTemplateSpec) string { return ComputeHashWithCollisions(tpl, 0) }

// ComputeHashWithCollisions also mixes in the number of hash collisions
// seen, so a name taken by another template yields a different hash.
func ComputeHashWithCollisions(tpl *corev1.PodTemplateSpec, collisions int32) string {
	data, _ := json.Marshal(tpl)
	h := fnv.New32a()
	_, _ = h.Write(data)
	if collisions > 0 {
		_, _ = h.Write([]byte(fmt.Sprint(collisions)))
	}
	return safeEncode(fmt.Sprint(h.Sum32()))
}

// safeEncode maps digits onto consonants and digits that cannot spell words.
func safeEncode(s string) string {
	const alphabet = "bcdfghjklmnpqrstvwxz2456789"
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		out[i] = alphabet[int(s[i])%len(alphabet)]
	}
	return string(out)
}
