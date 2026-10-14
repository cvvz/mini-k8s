// Package storage talks to etcd through its v3 JSON gateway: key/value CRUD
// with compare-and-swap, leases, and prefix watches. It knows nothing about
// API objects; values are opaque bytes.
package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Errors returned by Store.
var (
	ErrNotFound      = errors.New("key not found")
	ErrAlreadyExists = errors.New("key already exists")
	ErrConflict      = errors.New("revision conflict")
	ErrCompacted     = errors.New("requested revision has been compacted")
)

// KV is one stored value with its revisions.
type KV struct {
	Key            string
	Value          []byte
	CreateRevision int64
	ModRevision    int64
}

// Store is an etcd v3 JSON-gateway client.
type Store struct {
	url   string
	http  *http.Client
	watch *http.Client
}

// New returns a Store for an etcd client URL such as http://etcd:2379.
func New(url string) *Store {
	return &Store{url: url, http: &http.Client{Timeout: 10 * time.Second}, watch: &http.Client{}}
}

type rawKV struct {
	Key            string `json:"key"`
	Value          string `json:"value"`
	CreateRevision string `json:"create_revision"`
	ModRevision    string `json:"mod_revision"`
}

type header struct {
	Revision string `json:"revision"`
}

func (r rawKV) decode() (KV, error) {
	key, err := base64.StdEncoding.DecodeString(r.Key)
	if err != nil {
		return KV{}, err
	}
	value, err := base64.StdEncoding.DecodeString(r.Value)
	if err != nil {
		return KV{}, err
	}
	kv := KV{Key: string(key), Value: value}
	kv.CreateRevision, _ = strconv.ParseInt(r.CreateRevision, 10, 64)
	kv.ModRevision, _ = strconv.ParseInt(r.ModRevision, 10, 64)
	return kv, nil
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// prefixEnd returns the range end covering every key with the given prefix.
func prefixEnd(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return "\x00"
}

func (s *Store) call(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("etcd %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("etcd %s: %s: %s", path, resp.Status, bytes.TrimSpace(msg))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Get returns the value stored at key.
func (s *Store) Get(ctx context.Context, key string) (KV, error) {
	var out struct {
		KVs []rawKV `json:"kvs"`
	}
	if err := s.call(ctx, "/v3/kv/range", map[string]string{"key": b64(key)}, &out); err != nil {
		return KV{}, err
	}
	if len(out.KVs) == 0 {
		return KV{}, ErrNotFound
	}
	return out.KVs[0].decode()
}

// List returns every key under prefix and the store revision of the read.
func (s *Store) List(ctx context.Context, prefix string) ([]KV, int64, error) {
	var out struct {
		Header header  `json:"header"`
		KVs    []rawKV `json:"kvs"`
	}
	if err := s.call(ctx, "/v3/kv/range", map[string]string{"key": b64(prefix), "range_end": b64(prefixEnd(prefix))}, &out); err != nil {
		return nil, 0, err
	}
	rev, _ := strconv.ParseInt(out.Header.Revision, 10, 64)
	kvs := make([]KV, 0, len(out.KVs))
	for _, raw := range out.KVs {
		kv, err := raw.decode()
		if err != nil {
			return nil, 0, err
		}
		kvs = append(kvs, kv)
	}
	return kvs, rev, nil
}

// Revision returns the current store revision.
func (s *Store) Revision(ctx context.Context) (int64, error) {
	var out struct {
		Header header `json:"header"`
	}
	if err := s.call(ctx, "/v3/kv/range", map[string]any{"key": b64("\x00"), "count_only": true}, &out); err != nil {
		return 0, err
	}
	return strconv.ParseInt(out.Header.Revision, 10, 64)
}

type txnResult struct {
	Header    header `json:"header"`
	Succeeded bool   `json:"succeeded"`
	Responses []struct {
		ResponseRange *struct {
			KVs []rawKV `json:"kvs"`
		} `json:"response_range"`
	} `json:"responses"`
}

// txn runs "if compare then op else range(key)" and returns the outcome.
func (s *Store) txn(ctx context.Context, key string, compare map[string]any, op map[string]any) (txnResult, error) {
	compare["key"] = b64(key)
	compare["result"] = "EQUAL"
	var out txnResult
	err := s.call(ctx, "/v3/kv/txn", map[string]any{
		"compare": []any{compare},
		"success": []any{op},
		"failure": []any{map[string]any{"request_range": map[string]string{"key": b64(key)}}},
	}, &out)
	return out, err
}

func putOp(key string, value []byte, lease int64) map[string]any {
	put := map[string]any{"key": b64(key), "value": base64.StdEncoding.EncodeToString(value)}
	if lease != 0 {
		put["lease"] = strconv.FormatInt(lease, 10)
	}
	return map[string]any{"request_put": put}
}

// Create stores value only if key does not exist and returns the new revision.
// A nonzero lease attaches the key to that lease.
func (s *Store) Create(ctx context.Context, key string, value []byte, lease int64) (int64, error) {
	out, err := s.txn(ctx, key, map[string]any{"target": "CREATE", "create_revision": "0"}, putOp(key, value, lease))
	if err != nil {
		return 0, err
	}
	if !out.Succeeded {
		return 0, ErrAlreadyExists
	}
	return strconv.ParseInt(out.Header.Revision, 10, 64)
}

// Update replaces value if key's mod revision equals expected.
func (s *Store) Update(ctx context.Context, key string, value []byte, expected, lease int64) (int64, error) {
	out, err := s.txn(ctx, key, map[string]any{"target": "MOD", "mod_revision": strconv.FormatInt(expected, 10)}, putOp(key, value, lease))
	if err != nil {
		return 0, err
	}
	if !out.Succeeded {
		return 0, failureReason(out)
	}
	return strconv.ParseInt(out.Header.Revision, 10, 64)
}

// Delete removes key if its mod revision equals expected.
func (s *Store) Delete(ctx context.Context, key string, expected int64) (int64, error) {
	op := map[string]any{"request_delete_range": map[string]string{"key": b64(key)}}
	out, err := s.txn(ctx, key, map[string]any{"target": "MOD", "mod_revision": strconv.FormatInt(expected, 10)}, op)
	if err != nil {
		return 0, err
	}
	if !out.Succeeded {
		return 0, failureReason(out)
	}
	return strconv.ParseInt(out.Header.Revision, 10, 64)
}

func failureReason(out txnResult) error {
	if len(out.Responses) == 1 && out.Responses[0].ResponseRange != nil && len(out.Responses[0].ResponseRange.KVs) == 0 {
		return ErrNotFound
	}
	return ErrConflict
}

// GrantLease creates a lease that expires after ttl and returns its ID.
func (s *Store) GrantLease(ctx context.Context, ttl time.Duration) (int64, error) {
	var out struct {
		ID string `json:"ID"`
	}
	if err := s.call(ctx, "/v3/lease/grant", map[string]any{"TTL": strconv.FormatInt(int64(ttl/time.Second), 10)}, &out); err != nil {
		return 0, err
	}
	return strconv.ParseInt(out.ID, 10, 64)
}

// Health verifies that etcd serves reads.
func (s *Store) Health(ctx context.Context) error {
	_, err := s.Revision(ctx)
	return err
}

// EventType is PUT or DELETE.
type EventType string

// Watch event types.
const (
	Put    EventType = "PUT"
	Delete EventType = "DELETE"
)

// Event is one change observed by Watch. For deletes, KV carries the
// deletion revision and Prev the last value.
type Event struct {
	Type EventType
	KV   KV
	Prev *KV
}

// Watch streams changes under prefix starting at revision rev (inclusive).
// The channel closes when ctx ends or the stream fails; the error channel
// then receives the reason (ErrCompacted if rev is no longer available).
func (s *Store) Watch(ctx context.Context, prefix string, rev int64) (<-chan Event, <-chan error, error) {
	req := map[string]any{"create_request": map[string]any{
		"key": b64(prefix), "range_end": b64(prefixEnd(prefix)),
		"start_revision": strconv.FormatInt(rev, 10), "prev_kv": true,
	}}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url+"/v3/watch", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	resp, err := s.watch.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("etcd watch: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, nil, fmt.Errorf("etcd watch: %s", resp.Status)
	}
	decoder := json.NewDecoder(resp.Body)
	// The first message confirms creation or reports compaction.
	first, err := readWatchMessage(decoder)
	if err != nil {
		resp.Body.Close()
		return nil, nil, err
	}
	events := make(chan Event)
	errs := make(chan error, 1)
	go func() {
		defer resp.Body.Close()
		defer close(events)
		msg := first
		for {
			for _, raw := range msg.Events {
				ev, err := raw.decode()
				if err != nil {
					errs <- err
					return
				}
				select {
				case events <- ev:
				case <-ctx.Done():
					errs <- ctx.Err()
					return
				}
			}
			next, err := readWatchMessage(decoder)
			if err != nil {
				if ctx.Err() != nil {
					err = ctx.Err()
				}
				errs <- err
				return
			}
			msg = next
		}
	}()
	return events, errs, nil
}

type watchMessage struct {
	Created         bool       `json:"created"`
	Canceled        bool       `json:"canceled"`
	CompactRevision string     `json:"compact_revision"`
	CancelReason    string     `json:"cancel_reason"`
	Events          []rawEvent `json:"events"`
}

type rawEvent struct {
	Type   string `json:"type"`
	KV     rawKV  `json:"kv"`
	PrevKV *rawKV `json:"prev_kv"`
}

func (r rawEvent) decode() (Event, error) {
	kv, err := r.KV.decode()
	if err != nil {
		return Event{}, err
	}
	ev := Event{Type: Put, KV: kv}
	if r.Type == "DELETE" {
		ev.Type = Delete
	}
	if r.PrevKV != nil {
		prev, err := r.PrevKV.decode()
		if err != nil {
			return Event{}, err
		}
		ev.Prev = &prev
	}
	return ev, nil
}

func readWatchMessage(d *json.Decoder) (watchMessage, error) {
	var envelope struct {
		Result *watchMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := d.Decode(&envelope); err != nil {
		return watchMessage{}, fmt.Errorf("etcd watch stream: %w", err)
	}
	if envelope.Error != nil {
		return watchMessage{}, fmt.Errorf("etcd watch: %s", envelope.Error.Message)
	}
	if envelope.Result == nil {
		return watchMessage{}, errors.New("etcd watch: empty message")
	}
	if envelope.Result.CompactRevision != "" && envelope.Result.CompactRevision != "0" {
		return watchMessage{}, ErrCompacted
	}
	if envelope.Result.Canceled {
		return watchMessage{}, fmt.Errorf("etcd watch canceled: %s", envelope.Result.CancelReason)
	}
	return *envelope.Result, nil
}
