package service

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/storage/etcd3"
)

// leakRounds is how many consecutive rounds an allocated but unused value
// must be seen before it is released, so values reserved by a create that is
// still in flight are never taken away (Kubernetes uses the same grace).
const leakRounds = 3

// Repair reconciles the allocators with the stored Services, like the
// cluster IP and node port repair controllers of the Kubernetes API server:
// values in use are marked allocated, values leaked by failed writes are
// released, and Services stored before cluster IPs existed get one.
type Repair struct {
	store  *etcd3.Store
	prefix string
	alloc  *Allocators
	// ipLeaks and portLeaks count the rounds a value has been unused.
	ipLeaks, portLeaks map[string]int
}

// NewRepair returns a repair loop for the Services under prefix.
func NewRepair(store *etcd3.Store, prefix string, a *Allocators) *Repair {
	return &Repair{store: store, prefix: prefix, alloc: a, ipLeaks: map[string]int{}, portLeaks: map[string]int{}}
}

// Run repairs every interval until ctx ends.
func (r *Repair) Run(ctx context.Context, interval time.Duration) {
	for {
		if err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("service repair: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// RunOnce performs one reconciliation.
func (r *Repair) RunOnce(ctx context.Context) error {
	kvs, _, err := r.store.List(ctx, r.prefix+"/services/")
	if err != nil {
		return err
	}
	usedIPs, usedPorts := map[string]bool{}, map[int32]bool{}
	for _, kv := range kvs {
		var svc corev1.Service
		if err := json.Unmarshal(kv.Value, &svc); err != nil {
			return err
		}
		if svc.Spec.ClusterIP == "" && svc.Spec.Type != corev1.ServiceTypeExternalName {
			if err := r.migrate(ctx, kv, &svc); err != nil {
				log.Printf("service repair: assign a cluster IP to %s/%s: %v", svc.Namespace, svc.Name, err)
				continue
			}
		}
		if corev1.IsServiceIPSet(&svc) {
			usedIPs[svc.Spec.ClusterIP] = true
		}
		for _, p := range nodePorts(&svc.Spec) {
			usedPorts[p] = true
		}
	}
	allocatedIPs, err := r.alloc.IPs.Allocated(ctx)
	if err != nil {
		return err
	}
	for ip := range usedIPs {
		if !allocatedIPs[ip] {
			// In use but not recorded (the record was lost): take it back.
			if err := r.alloc.IPs.Allocate(ctx, ip); err != nil {
				log.Printf("service repair: cluster IP %s: %v", ip, err)
			}
		}
	}
	if err := r.alloc.IPs.Release(ctx, sweep(r.ipLeaks, allocatedIPs, usedIPs)...); err != nil {
		return err
	}
	allocatedPorts, err := r.alloc.Ports.Allocated(ctx)
	if err != nil {
		return err
	}
	for p := range usedPorts {
		if !allocatedPorts[p] {
			if err := r.alloc.Ports.Allocate(ctx, p); err != nil {
				log.Printf("service repair: node port %d: %v", p, err)
			}
		}
	}
	return r.alloc.Ports.Release(ctx, sweep(r.portLeaks, allocatedPorts, usedPorts)...)
}

// sweep counts how many rounds each allocated value has gone unused and
// returns the values unused for leakRounds rounds.
func sweep[K comparable](leaks map[string]int, allocated, used map[K]bool) []K {
	key := func(k K) string { b, _ := json.Marshal(k); return string(b) }
	seen := map[string]bool{}
	var out []K
	for v := range allocated {
		if used[v] {
			continue
		}
		k := key(v)
		seen[k] = true
		if leaks[k]++; leaks[k] >= leakRounds {
			out = append(out, v)
			delete(leaks, k)
		}
	}
	for k := range leaks {
		if !seen[k] {
			delete(leaks, k)
		}
	}
	return out
}

// migrate gives a Service stored before Services had cluster IPs its
// defaults, a cluster IP and node ports, writing it back with compare-and-swap.
func (r *Repair) migrate(ctx context.Context, kv etcd3.KV, svc *corev1.Service) error {
	corev1.SetDefaultsServiceSpec(&svc.Spec)
	finish, err := r.alloc.allocate(ctx, &svc.Spec, nil, false)
	if err != nil {
		return err
	}
	data, err := json.Marshal(svc)
	if err == nil {
		_, err = r.store.Update(ctx, kv.Key, data, kv.ModRevision, 0)
	}
	finish(ctx, err == nil)
	if errors.Is(err, etcd3.ErrConflict) || errors.Is(err, etcd3.ErrNotFound) {
		return errors.New("the Service changed; retrying in the next round")
	}
	return err
}
