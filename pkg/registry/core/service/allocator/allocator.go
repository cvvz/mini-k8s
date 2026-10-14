// Package allocator hands out the offsets of a fixed range, keeping the
// allocated set in a single etcd key updated with compare-and-swap, like
// the RangeAllocation objects behind the Service IP and node port
// allocators of the Kubernetes API server.
package allocator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"

	"mini-k8s/pkg/storage/etcd3"
)

// Errors returned for explicit and dynamic allocations.
var (
	ErrAllocated = errors.New("provided value is already allocated")
	ErrFull      = errors.New("range is full")
)

// Store is the part of the etcd client the allocator uses.
type Store interface {
	Get(ctx context.Context, key string) (etcd3.KV, error)
	Create(ctx context.Context, key string, value []byte, lease int64) (int64, error)
	Update(ctx context.Context, key string, value []byte, expected, lease int64) (int64, error)
}

// Range allocates the offsets 0..Size-1. Dynamic allocations prefer the
// offsets from Static on, keeping the start of the range for explicit
// requests (such as the cluster DNS address).
type Range struct {
	Store Store
	Key   string
	// Name describes the range in the stored record, e.g. "10.96.0.0/12".
	Name   string
	Size   int
	Static int
}

// record is the stored form.
type record struct {
	Range     string `json:"range"`
	Allocated []int  `json:"allocated"`
}

// Allocate reserves offset; ErrAllocated if it is taken.
func (r *Range) Allocate(ctx context.Context, offset int) error {
	if offset < 0 || offset >= r.Size {
		return fmt.Errorf("offset %d is outside the range", offset)
	}
	return r.update(ctx, func(set map[int]bool) (bool, error) {
		if set[offset] {
			return false, ErrAllocated
		}
		set[offset] = true
		return true, nil
	})
}

// AllocateNext reserves a free offset, preferring a random one outside the
// static band; ErrFull when none is left.
func (r *Range) AllocateNext(ctx context.Context) (int, error) {
	var picked int
	err := r.update(ctx, func(set map[int]bool) (bool, error) {
		for _, band := range [][2]int{{r.Static, r.Size}, {0, r.Static}} {
			n := band[1] - band[0]
			if n <= 0 {
				continue
			}
			start := rand.IntN(n)
			for i := range n {
				if o := band[0] + (start+i)%n; !set[o] {
					set[o], picked = true, o
					return true, nil
				}
			}
		}
		return false, ErrFull
	})
	return picked, err
}

// Release frees offsets; offsets that are not allocated are ignored.
func (r *Range) Release(ctx context.Context, offsets ...int) error {
	if len(offsets) == 0 {
		return nil
	}
	return r.update(ctx, func(set map[int]bool) (bool, error) {
		changed := false
		for _, o := range offsets {
			if set[o] {
				delete(set, o)
				changed = true
			}
		}
		return changed, nil
	})
}

// Allocated returns the allocated offsets.
func (r *Range) Allocated(ctx context.Context) (map[int]bool, error) {
	set, _, err := r.load(ctx)
	return set, err
}

func (r *Range) load(ctx context.Context) (map[int]bool, int64, error) {
	kv, err := r.Store.Get(ctx, r.Key)
	if errors.Is(err, etcd3.ErrNotFound) {
		return map[int]bool{}, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var rec record
	if err := json.Unmarshal(kv.Value, &rec); err != nil {
		return nil, 0, fmt.Errorf("decode %s: %w", r.Key, err)
	}
	set := make(map[int]bool, len(rec.Allocated))
	for _, o := range rec.Allocated {
		if o >= 0 && o < r.Size {
			set[o] = true
		}
	}
	return set, kv.ModRevision, nil
}

// update applies mutate to the current set and stores the result if mutate
// reports a change, retrying when a concurrent writer got there first.
func (r *Range) update(ctx context.Context, mutate func(map[int]bool) (bool, error)) error {
	for {
		set, rev, err := r.load(ctx)
		if err != nil {
			return err
		}
		changed, err := mutate(set)
		if err != nil || !changed {
			return err
		}
		rec := record{Range: r.Name, Allocated: make([]int, 0, len(set))}
		for o := range set {
			rec.Allocated = append(rec.Allocated, o)
		}
		sort.Ints(rec.Allocated)
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if rev == 0 {
			_, err = r.Store.Create(ctx, r.Key, data, 0)
		} else {
			_, err = r.Store.Update(ctx, r.Key, data, rev, 0)
		}
		if errors.Is(err, etcd3.ErrAlreadyExists) || errors.Is(err, etcd3.ErrConflict) || errors.Is(err, etcd3.ErrNotFound) {
			continue
		}
		return err
	}
}
