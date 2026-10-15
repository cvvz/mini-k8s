package leaderelection

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/test/integration/framework"
)

// candidate runs an elector and records what happens to it.
type candidate struct {
	id      string
	leading atomic.Bool
	stopped atomic.Bool
	mu      sync.Mutex
	leaders []string
	cancel  context.CancelFunc
	done    chan struct{}
}

func start(c *client.Client, id string, release bool, lease time.Duration) *candidate {
	ctx, cancel := context.WithCancel(context.Background())
	cd := &candidate{id: id, cancel: cancel, done: make(chan struct{})}
	cfg := Config{Client: c, Namespace: "kube-system", Name: "test-lock", Identity: id, LeaseDuration: lease,
		RenewDeadline: lease / 2, RetryPeriod: 100 * time.Millisecond, ReleaseOnCancel: release,
		Callbacks: Callbacks{
			OnStartedLeading: func(ctx context.Context) {
				cd.leading.Store(true)
				<-ctx.Done()
				cd.leading.Store(false)
			},
			OnStoppedLeading: func() { cd.stopped.Store(true) },
			OnNewLeader: func(id string) {
				cd.mu.Lock()
				cd.leaders = append(cd.leaders, id)
				cd.mu.Unlock()
			},
		}}
	go func() {
		defer close(cd.done)
		Run(ctx, cfg)
	}()
	return cd
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func holder(t *testing.T, c *client.Client) (string, int32) {
	t.Helper()
	l, err := client.Leases(c, "kube-system").Get(context.Background(), "test-lock")
	if err != nil {
		return "", -1
	}
	return ptr.Deref(l.Spec.HolderIdentity, ""), ptr.Deref(l.Spec.LeaseTransitions, 0)
}

func TestFailoverOnRelease(t *testing.T) {
	c := framework.StartTestServer(t)
	a := start(c, "a", true, 2*time.Second)
	waitFor(t, "a to lead", a.leading.Load)
	b := start(c, "b", true, 2*time.Second)
	t.Cleanup(b.cancel)
	time.Sleep(500 * time.Millisecond)
	if b.leading.Load() {
		t.Fatal("two leaders")
	}
	if h, n := holder(t, c); h != "a" || n != 0 {
		t.Fatalf("lease: %s %d", h, n)
	}
	waitFor(t, "b to see a lead", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.leaders) > 0 && b.leaders[0] == "a"
	})

	// a steps down and releases the Lease: b takes over at once, well
	// before the 2s lease would expire.
	stoppedAt := time.Now()
	a.cancel()
	<-a.done
	if !a.stopped.Load() || a.leading.Load() {
		t.Fatal("a must stop leading")
	}
	waitFor(t, "b to lead", b.leading.Load)
	if d := time.Since(stoppedAt); d > 1500*time.Millisecond {
		t.Fatalf("failover took %v", d)
	}
	if h, n := holder(t, c); h != "b" || n != 1 {
		t.Fatalf("lease after failover: %s %d", h, n)
	}
}

func TestFailoverAfterExpiry(t *testing.T) {
	c := framework.StartTestServer(t)
	a := start(c, "a", false, 2*time.Second)
	waitFor(t, "a to lead", a.leading.Load)
	b := start(c, "b", false, 2*time.Second)
	t.Cleanup(b.cancel)
	// a vanishes without releasing: b waits for the lease to expire.
	a.cancel()
	<-a.done
	if h, _ := holder(t, c); h != "a" {
		t.Fatalf("a did not release: %s", h)
	}
	time.Sleep(time.Second)
	if b.leading.Load() {
		t.Fatal("b took an unexpired lease")
	}
	waitFor(t, "b to lead after expiry", b.leading.Load)
}

func TestLeaderStepsDownWhenItCannotRenew(t *testing.T) {
	c := framework.StartTestServer(t)
	a := start(c, "a", true, 2*time.Second)
	t.Cleanup(a.cancel)
	waitFor(t, "a to lead", a.leading.Load)
	// Someone else takes the Lease for a long time.
	ctx := context.Background()
	l, err := client.Leases(c, "kube-system").Get(ctx, "test-lock")
	if err != nil {
		t.Fatal(err)
	}
	l.Spec.HolderIdentity, l.Spec.LeaseDurationSeconds = ptr.To("intruder"), ptr.To[int32](3600)
	if _, err := client.Leases(c, "kube-system").Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a to stop leading", func() bool { return !a.leading.Load() && a.stopped.Load() })
	if h, _ := holder(t, c); h != "intruder" {
		t.Fatalf("a must not release a Lease it does not hold: %s", h)
	}
}
