// Package workqueue is a deduplicating, rate-limited work queue with the
// semantics of client-go's: a key is never processed by two workers at once,
// and a key added while it is being processed is queued again after Done.
package workqueue

import (
	"context"
	"log"
	"sync"
	"time"
)

// Queue holds string keys (usually "namespace/name").
type Queue struct {
	mu         sync.Mutex
	cond       *sync.Cond
	queue      []string
	dirty      map[string]bool
	processing map[string]bool
	failures   map[string]int
	shutdown   bool

	baseDelay, maxDelay time.Duration
}

// New returns a queue whose retry delay doubles from 10ms up to 30s.
func New() *Queue {
	q := &Queue{
		dirty: map[string]bool{}, processing: map[string]bool{}, failures: map[string]int{},
		baseDelay: 10 * time.Millisecond, maxDelay: 30 * time.Second,
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Add marks key as needing processing.
func (q *Queue) Add(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shutdown || q.dirty[key] {
		return
	}
	q.dirty[key] = true
	if q.processing[key] {
		return
	}
	q.queue = append(q.queue, key)
	q.cond.Signal()
}

// AddAfter adds key once d has elapsed.
func (q *Queue) AddAfter(key string, d time.Duration) {
	if d <= 0 {
		q.Add(key)
		return
	}
	time.AfterFunc(d, func() { q.Add(key) })
}

// AddRateLimited re-adds key with exponential backoff per key.
func (q *Queue) AddRateLimited(key string) {
	q.mu.Lock()
	n := q.failures[key]
	q.failures[key] = n + 1
	q.mu.Unlock()
	delay := q.baseDelay << min(n, 20)
	if delay > q.maxDelay || delay <= 0 {
		delay = q.maxDelay
	}
	q.AddAfter(key, delay)
}

// Forget resets the backoff of key after a successful sync.
func (q *Queue) Forget(key string) {
	q.mu.Lock()
	delete(q.failures, key)
	q.mu.Unlock()
}

// Get blocks for the next key; ok is false after ShutDown.
func (q *Queue) Get() (key string, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.queue) == 0 && !q.shutdown {
		q.cond.Wait()
	}
	if len(q.queue) == 0 {
		return "", false
	}
	key, q.queue = q.queue[0], q.queue[1:]
	q.processing[key] = true
	delete(q.dirty, key)
	return key, true
}

// Done marks key as processed, requeueing it if it was added meanwhile.
func (q *Queue) Done(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.processing, key)
	if q.dirty[key] {
		q.queue = append(q.queue, key)
		q.cond.Signal()
	}
}

// Len returns the number of keys waiting.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queue)
}

// ShutDown wakes all workers and makes Get return false.
func (q *Queue) ShutDown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.shutdown = true
	q.cond.Broadcast()
}

// Run processes keys with n workers until ctx is done. A sync error requeues
// the key with backoff; success forgets its failure history.
func Run(ctx context.Context, q *Queue, name string, workers int, handler func(ctx context.Context, key string) error) {
	go func() {
		<-ctx.Done()
		q.ShutDown()
	}()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				key, ok := q.Get()
				if !ok {
					return
				}
				if err := handler(ctx, key); err != nil {
					if ctx.Err() == nil {
						log.Printf("%s: sync %q failed: %v", name, key, err)
					}
					q.AddRateLimited(key)
				} else {
					q.Forget(key)
				}
				q.Done(key)
			}
		}()
	}
	wg.Wait()
}
