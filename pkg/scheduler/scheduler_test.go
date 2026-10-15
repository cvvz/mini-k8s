package scheduler

import (
	"testing"
	"time"
)

func TestBackoffIsBounded(t *testing.T) {
	for attempts, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 4: 8 * time.Second, 5: 10 * time.Second,
		40: 10 * time.Second, 100: 10 * time.Second, 1 << 20: 10 * time.Second} {
		if got := Backoff(attempts); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}
