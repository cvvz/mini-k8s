package workqueue

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeduplicatesAndRequeuesDuringProcessing(t *testing.T) {
	q := New()
	q.Add("a")
	q.Add("a")
	q.Add("b")
	if q.Len() != 2 {
		t.Fatalf("duplicates queued: %d", q.Len())
	}
	key, _ := q.Get()
	if key != "a" {
		t.Fatalf("FIFO broken: %s", key)
	}
	q.Add("a") // added while processing: must not be handed to another worker yet
	if k, _ := q.Get(); k != "b" {
		t.Fatalf("a was handed out twice: %s", k)
	}
	q.Done("b")
	if q.Len() != 0 {
		t.Fatal("a must wait until Done")
	}
	q.Done("a")
	if k, _ := q.Get(); k != "a" {
		t.Fatalf("a not requeued after Done: %s", k)
	}
}

func TestShutDownUnblocksGet(t *testing.T) {
	q := New()
	done := make(chan bool)
	go func() {
		_, ok := q.Get()
		done <- ok
	}()
	time.Sleep(20 * time.Millisecond)
	q.ShutDown()
	if <-done {
		t.Fatal("Get returned ok after shutdown")
	}
}

func TestRunRetriesWithBackoff(t *testing.T) {
	q := New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var calls atomic.Int32
	finished := make(chan struct{})
	go Run(ctx, q, "test", 2, func(ctx context.Context, key string) error {
		if calls.Add(1) < 3 {
			return errors.New("transient")
		}
		close(finished)
		return nil
	})
	q.Add("k")
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatalf("not retried to success, calls=%d", calls.Load())
	}
	time.Sleep(50 * time.Millisecond)
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.failures) != 0 {
		t.Fatal("success must forget failures")
	}
}
