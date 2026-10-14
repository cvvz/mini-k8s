package allocator

import (
	"context"
	"net"
	"sync"
	"testing"

	"mini-k8s/pkg/storage/etcd3"
)

// memStore is an in-memory Store with etcd's compare-and-swap semantics.
type memStore struct {
	mu  sync.Mutex
	rev int64
	kvs map[string]etcd3.KV
}

func newMemStore() *memStore { return &memStore{kvs: map[string]etcd3.KV{}} }

func (m *memStore) Get(_ context.Context, key string) (etcd3.KV, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kv, ok := m.kvs[key]
	if !ok {
		return etcd3.KV{}, etcd3.ErrNotFound
	}
	return kv, nil
}

func (m *memStore) Create(_ context.Context, key string, value []byte, _ int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.kvs[key]; ok {
		return 0, etcd3.ErrAlreadyExists
	}
	m.rev++
	m.kvs[key] = etcd3.KV{Key: key, Value: value, CreateRevision: m.rev, ModRevision: m.rev}
	return m.rev, nil
}

func (m *memStore) Update(_ context.Context, key string, value []byte, expected, _ int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kv, ok := m.kvs[key]
	if !ok {
		return 0, etcd3.ErrNotFound
	}
	if kv.ModRevision != expected {
		return 0, etcd3.ErrConflict
	}
	m.rev++
	kv.Value, kv.ModRevision = value, m.rev
	m.kvs[key] = kv
	return m.rev, nil
}

func TestIPs(t *testing.T) {
	ctx := context.Background()
	a, err := NewIPs(newMemStore(), "/ranges/serviceips", "10.96.0.0/12")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Allocate(ctx, "10.96.0.10"); err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]string{
		"10.96.0.10":     "provided IP is already allocated",
		"10.112.0.5":     "the provided network does not match the current range",
		"10.96.0.0":      "the provided IP is the network or broadcast address of the range",
		"10.111.255.255": "the provided IP is the network or broadcast address of the range",
	} {
		if err := a.Allocate(ctx, ip); err == nil || err.Error() != want {
			t.Errorf("Allocate(%s) = %v, want %q", ip, err, want)
		}
	}
	if err := a.Check(ctx, "10.96.0.10"); err == nil {
		t.Error("Check must report an allocated address")
	}
	_, static, _ := net.ParseCIDR("10.96.0.0/24") // the first 256 addresses
	for range 50 {
		ip, err := a.AllocateNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if static.Contains(net.ParseIP(ip)) {
			t.Fatalf("dynamic allocation %s fell into the static band", ip)
		}
	}
	if err := a.Release(ctx, "10.96.0.10", "192.168.0.1"); err != nil {
		t.Fatal(err)
	}
	set, err := a.Allocated(ctx)
	if err != nil || len(set) != 50 || set["10.96.0.10"] {
		t.Fatalf("Allocated() = %d addresses, %v", len(set), err)
	}
}

func TestIPsFillTinyRangeConcurrently(t *testing.T) {
	ctx := context.Background()
	a, err := NewIPs(newMemStore(), "/ranges/serviceips", "10.0.0.0/27") // 30 addresses, no static band
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			ip, err := a.AllocateNext(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[ip] {
				t.Errorf("%s allocated twice", ip)
			}
			seen[ip] = true
		})
	}
	wg.Wait()
	if _, err := a.AllocateNext(ctx); err == nil || err.Error() != "failed to allocate a serviceIP: range is full" {
		t.Fatalf("full range: %v", err)
	}
}

func TestPorts(t *testing.T) {
	ctx := context.Background()
	a := NewPorts(newMemStore(), "/ranges/servicenodeports", 30000, 32767)
	if err := a.Allocate(ctx, 30080); err != nil {
		t.Fatal(err)
	}
	if err := a.Allocate(ctx, 30080); err == nil || err.Error() != "provided port is already allocated" {
		t.Fatal(err)
	}
	if err := a.Allocate(ctx, 40000); err == nil ||
		err.Error() != "provided port is not in the valid range. The range of valid ports is 30000-32767" {
		t.Fatal(err)
	}
	for range 20 {
		p, err := a.AllocateNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if p < 30086 || p > 32767 {
			t.Fatalf("dynamic node port %d outside 30086-32767", p)
		}
	}
	if err := a.Release(ctx, 30080); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx, 30080); err != nil {
		t.Fatalf("released port is free again: %v", err)
	}
}
