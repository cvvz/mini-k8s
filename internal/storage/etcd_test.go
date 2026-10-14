package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"mini-k8s/internal/storage/etcdtest"
)

func TestCRUDWithRevisions(t *testing.T) {
	s := New(etcdtest.URL(t))
	ctx := context.Background()
	p := etcdtest.Prefix(t)

	rev, err := s.Create(ctx, p+"/a", []byte("1"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, p+"/a", []byte("again"), 0); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate create: %v", err)
	}
	kv, err := s.Get(ctx, p+"/a")
	if err != nil || string(kv.Value) != "1" || kv.ModRevision != rev || kv.CreateRevision != rev {
		t.Fatalf("get: %+v %v", kv, err)
	}
	if _, err := s.Update(ctx, p+"/a", []byte("2"), rev-1, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}
	rev2, err := s.Update(ctx, p+"/a", []byte("2"), rev, 0)
	if err != nil || rev2 <= rev {
		t.Fatalf("update: %d %v", rev2, err)
	}
	if _, err := s.Update(ctx, p+"/missing", []byte("x"), 5, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	if _, err := s.Create(ctx, p+"/b", []byte("b"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, p+"x/c", []byte("outside"), 0); err != nil {
		t.Fatal(err)
	}
	kvs, listRev, err := s.List(ctx, p+"/")
	if err != nil || len(kvs) != 2 || listRev < rev2 {
		t.Fatalf("list: %d items rev %d %v", len(kvs), listRev, err)
	}
	if _, err := s.Delete(ctx, p+"/a", rev); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale delete: %v", err)
	}
	if _, err := s.Delete(ctx, p+"/a", rev2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, p+"/a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted key still readable: %v", err)
	}
}

func TestWatchDeliversPutsAndDeletesWithPrevValue(t *testing.T) {
	s := New(etcdtest.URL(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := etcdtest.Prefix(t) + "/"
	start, err := s.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	events, errs, err := s.Watch(ctx, p, start+1)
	if err != nil {
		t.Fatal(err)
	}
	rev, _ := s.Create(ctx, p+"k", []byte("v1"), 0)
	rev, _ = s.Update(ctx, p+"k", []byte("v2"), rev, 0)
	if _, err := s.Delete(ctx, p+"k", rev); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		typ   EventType
		value string
		prev  string
	}{{Put, "v1", ""}, {Put, "v2", "v1"}, {Delete, "", "v2"}}
	for i, w := range want {
		select {
		case ev := <-events:
			if ev.Type != w.typ || string(ev.KV.Value) != w.value || (w.prev != "" && (ev.Prev == nil || string(ev.Prev.Value) != w.prev)) {
				t.Fatalf("event %d: %+v", i, ev)
			}
		case err := <-errs:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("timed out waiting for watch event")
		}
	}
}

func TestLeaseExpiresKey(t *testing.T) {
	s := New(etcdtest.URL(t))
	ctx := context.Background()
	p := etcdtest.Prefix(t)
	lease, err := s.GrantLease(ctx, 2*time.Second)
	if err != nil || lease == 0 {
		t.Fatalf("lease: %d %v", lease, err)
	}
	if _, err := s.Create(ctx, p+"/ttl", []byte("x"), lease); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := s.Get(ctx, p+"/ttl"); errors.Is(err, ErrNotFound) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("leased key did not expire")
}
