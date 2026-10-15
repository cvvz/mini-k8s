// Package leaderelection elects one leader among replicas of a component
// with a Lease, like k8s.io/client-go/tools/leaderelection: candidates try
// to take the Lease every RetryPeriod; the leader renews it and stops
// leading when it cannot renew within RenewDeadline; others wait until it
// has not been renewed for LeaseDuration, judged by their own clock since
// they last saw it change (so clock skew between machines does not matter).
package leaderelection

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math/rand"
	"sync"
	"time"

	coordinationv1 "mini-k8s/pkg/apis/coordination/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/util/ptr"
)

// Callbacks are called as leadership changes.
type Callbacks struct {
	// OnStartedLeading runs the component; its context ends when
	// leadership is lost or the elector stops.
	OnStartedLeading func(ctx context.Context)
	// OnStoppedLeading is called when Run returns (also when the elector
	// never led).
	OnStoppedLeading func()
	// OnNewLeader reports every leader observed, this one included.
	OnNewLeader func(identity string)
}

// Config describes an election.
type Config struct {
	Client          *client.Client
	Namespace, Name string // the Lease
	Identity        string
	LeaseDuration   time.Duration
	RenewDeadline   time.Duration
	RetryPeriod     time.Duration
	// ReleaseOnCancel gives the Lease up when ctx ends, so another
	// candidate takes over at once.
	ReleaseOnCancel bool
	// Recorder, when set, records LeaderElection events on the Lease.
	Recorder  *record.Recorder
	Callbacks Callbacks
}

// Defaults of kube-controller-manager and kube-scheduler.
const (
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewDeadline = 10 * time.Second
	DefaultRetryPeriod   = 2 * time.Second
)

type elector struct {
	cfg Config
	now func() time.Time

	mu             sync.Mutex
	observed       coordinationv1.LeaseSpec
	observedRaw    []byte
	observedTime   time.Time
	lease          *coordinationv1.Lease
	reportedLeader string
}

// Run takes part in the election until ctx ends or leadership is lost.
func Run(ctx context.Context, cfg Config) {
	e := &elector{cfg: cfg, now: time.Now}
	e.run(ctx)
}

func (e *elector) run(ctx context.Context) {
	defer func() {
		if e.cfg.Callbacks.OnStoppedLeading != nil {
			e.cfg.Callbacks.OnStoppedLeading()
		}
	}()
	if !e.acquire(ctx) {
		return
	}
	leading, stop := context.WithCancel(ctx)
	defer stop()
	go e.cfg.Callbacks.OnStartedLeading(leading)
	e.renew(leading)
}

// jittered waits about d (up to 2.2 d, as client-go's JitterFactor 1.2).
func jittered(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d + time.Duration(rand.Float64()*1.2*float64(d))):
		return true
	}
}

// acquire tries every RetryPeriod until it holds the Lease or ctx ends.
func (e *elector) acquire(ctx context.Context) bool {
	for {
		ok := e.tryAcquireOrRenew(ctx)
		e.maybeReportTransition()
		if ok {
			e.event("became leader")
			log.Printf("leaderelection: %s acquired lease %s/%s", e.cfg.Identity, e.cfg.Namespace, e.cfg.Name)
			return true
		}
		if !jittered(ctx, e.cfg.RetryPeriod) {
			return false
		}
	}
}

// renew keeps the Lease until a renewal fails for RenewDeadline or ctx
// ends; then it releases the Lease if asked to.
func (e *elector) renew(ctx context.Context) {
	defer func() {
		if e.cfg.ReleaseOnCancel {
			e.release()
		}
	}()
	for ctx.Err() == nil {
		deadline := time.Now().Add(e.cfg.RenewDeadline)
		renewed := false
		for !renewed && time.Now().Before(deadline) && ctx.Err() == nil {
			attempt, cancel := context.WithDeadline(ctx, deadline)
			renewed = e.tryAcquireOrRenew(attempt)
			cancel()
			if !renewed {
				select {
				case <-ctx.Done():
				case <-time.After(e.cfg.RetryPeriod):
				}
			}
		}
		e.maybeReportTransition()
		if !renewed {
			if ctx.Err() == nil {
				e.event("stopped leading")
				log.Printf("leaderelection: %s failed to renew lease %s/%s", e.cfg.Identity, e.cfg.Namespace, e.cfg.Name)
			}
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(e.cfg.RetryPeriod):
		}
	}
}

func (e *elector) isLeader() bool {
	return ptr.Deref(e.observed.HolderIdentity, "") == e.cfg.Identity
}

func (e *elector) setObserved(spec coordinationv1.LeaseSpec) {
	e.observed, e.observedTime = spec, e.now()
}

// tryAcquireOrRenew creates the Lease, takes it over once expired, or
// renews it while held.
func (e *elector) tryAcquireOrRenew(ctx context.Context) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	mt := metav1.MicroTime{Time: now}
	want := coordinationv1.LeaseSpec{HolderIdentity: ptr.To(e.cfg.Identity),
		LeaseDurationSeconds: ptr.To(int32(e.cfg.LeaseDuration / time.Second)), AcquireTime: &mt, RenewTime: &mt, LeaseTransitions: ptr.To[int32](0)}
	leases := client.Leases(e.cfg.Client, e.cfg.Namespace)
	current, err := leases.Get(ctx, e.cfg.Name)
	if client.IsNotFound(err) {
		created, err := leases.Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: e.cfg.Name, Namespace: e.cfg.Namespace},
			Spec: want})
		if err != nil {
			return false
		}
		e.lease = created
		e.setObserved(want)
		return true
	}
	if err != nil {
		return false
	}
	raw, _ := json.Marshal(current.Spec)
	if string(raw) != string(e.observedRaw) {
		e.setObserved(current.Spec)
		e.observedRaw = raw
	}
	e.lease = current
	duration := time.Duration(ptr.Deref(current.Spec.LeaseDurationSeconds, 0)) * time.Second
	if ptr.Deref(current.Spec.HolderIdentity, "") != "" && e.observedTime.Add(duration).After(now) && !e.isLeader() {
		return false // held by another and not expired
	}
	if e.isLeader() {
		want.AcquireTime, want.LeaseTransitions = current.Spec.AcquireTime, current.Spec.LeaseTransitions
	} else {
		want.LeaseTransitions = ptr.To(ptr.Deref(current.Spec.LeaseTransitions, 0) + 1)
	}
	update := *current
	update.Spec = want
	updated, err := leases.Update(ctx, &update)
	if err != nil {
		return false
	}
	e.lease = updated
	e.setObserved(want)
	return true
}

// release hands the Lease back with a one second duration.
func (e *elector) release() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.isLeader() || e.lease == nil {
		return
	}
	mt := metav1.MicroTime{Time: e.now()}
	update := *e.lease
	update.Spec = coordinationv1.LeaseSpec{LeaseDurationSeconds: ptr.To[int32](1), AcquireTime: &mt, RenewTime: &mt,
		LeaseTransitions: e.observed.LeaseTransitions}
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.RenewDeadline)
	defer cancel()
	if _, err := client.Leases(e.cfg.Client, e.cfg.Namespace).Update(ctx, &update); err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("leaderelection: release %s/%s: %v", e.cfg.Namespace, e.cfg.Name, err)
		}
		return
	}
	e.setObserved(update.Spec)
	log.Printf("leaderelection: %s released lease %s/%s", e.cfg.Identity, e.cfg.Namespace, e.cfg.Name)
}

func (e *elector) maybeReportTransition() {
	e.mu.Lock()
	holder := ptr.Deref(e.observed.HolderIdentity, "")
	if holder == e.reportedLeader {
		e.mu.Unlock()
		return
	}
	e.reportedLeader = holder
	e.mu.Unlock()
	if e.cfg.Callbacks.OnNewLeader != nil {
		go e.cfg.Callbacks.OnNewLeader(holder)
	}
}

// event records "<identity> <what>" on the Lease, as client-go's
// LeaseLock does.
func (e *elector) event(what string) {
	if e.cfg.Recorder == nil {
		return
	}
	e.mu.Lock()
	lease := e.lease
	e.mu.Unlock()
	if lease == nil {
		lease = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: e.cfg.Name, Namespace: e.cfg.Namespace}}
	}
	e.cfg.Recorder.Eventf(lease, corev1.EventTypeNormal, "LeaderElection", "%s %s", e.cfg.Identity, what)
}
