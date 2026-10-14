// Package controller contains the controller-manager's reconcilers.
package controller

import (
	"sync"
	"time"
)

// expectations stop a controller from acting on a stale cache: after it
// creates or deletes Pods it waits until the informer has observed those
// changes (or a timeout passes) before computing a new diff.
type expectations struct {
	mu sync.Mutex
	m  map[string]*expectation
}

type expectation struct {
	adds int
	dels map[string]bool
	at   time.Time
}

const expectationTimeout = time.Minute

func newExpectations() *expectations { return &expectations{m: map[string]*expectation{}} }

// expect records pending creations and the UIDs of pending deletions.
func (e *expectations) expect(key string, adds int, dels []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	x := &expectation{adds: adds, dels: map[string]bool{}, at: time.Now()}
	for _, uid := range dels {
		x.dels[uid] = true
	}
	e.m[key] = x
}

func (e *expectations) observeAdd(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if x := e.m[key]; x != nil {
		x.adds--
	}
}

func (e *expectations) observeDelete(key, uid string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if x := e.m[key]; x != nil {
		delete(x.dels, uid)
	}
}

func (e *expectations) satisfied(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	x := e.m[key]
	return x == nil || (x.adds <= 0 && len(x.dels) == 0) || time.Since(x.at) > expectationTimeout
}

func (e *expectations) forget(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.m, key)
}
