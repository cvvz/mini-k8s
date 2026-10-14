package mini

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

var ErrNotFound = errors.New("resource not found")
var ErrConflict = errors.New("resource version conflict")

type Store struct {
	URL  string
	HTTP *http.Client
}

func NewStore(url string) *Store { return &Store{url, &http.Client{Timeout: 5 * time.Second}} }
func (s *Store) call(ctx context.Context, method string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.URL+"/v3/kv/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("etcd %s: %s", resp.Status, b)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
func key(kind, name string) string {
	if kind == "Service" {
		name = "singleton"
	}
	return "/mini/" + kind + "/" + name
}

type rangeResult struct {
	KVs []struct {
		Value       string `json:"value"`
		ModRevision string `json:"mod_revision"`
	} `json:"kvs"`
}

func (s *Store) List(ctx context.Context, kind string) ([]Resource, error) {
	prefix := "/mini/" + kind + "/"
	end := prefix[:len(prefix)-1] + "0"
	var out rangeResult
	if err := s.call(ctx, "range", map[string]string{"key": b64(prefix), "range_end": b64(end)}, &out); err != nil {
		return nil, err
	}
	result := make([]Resource, 0, len(out.KVs))
	for _, kv := range out.KVs {
		b, err := base64.StdEncoding.DecodeString(kv.Value)
		if err != nil {
			return nil, err
		}
		var r Resource
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		r.Metadata.ResourceVersion, err = strconv.ParseInt(kv.ModRevision, 10, 64)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, nil
}
func (s *Store) Get(ctx context.Context, kind, name string) (Resource, error) {
	items, err := s.List(ctx, kind)
	if err != nil {
		return Resource{}, err
	}
	for _, r := range items {
		if r.Metadata.Name == name {
			return r, nil
		}
	}
	return Resource{}, ErrNotFound
}
func (s *Store) Write(ctx context.Context, r Resource, revision int64, del bool) (Resource, error) {
	k := key(r.Kind, r.Metadata.Name)
	b, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	op := map[string]any{"request_put": map[string]string{"key": b64(k), "value": b64(string(b))}}
	if del {
		op = map[string]any{"request_delete_range": map[string]string{"key": b64(k)}}
	}
	var out struct {
		Succeeded bool `json:"succeeded"`
		Header    struct {
			Revision string `json:"revision"`
		} `json:"header"`
	}
	err = s.call(ctx, "txn", map[string]any{
		"compare": []any{map[string]any{"key": b64(k), "target": "MOD", "result": "EQUAL", "mod_revision": strconv.FormatInt(revision, 10)}},
		"success": []any{op},
	}, &out)
	if err != nil {
		return r, err
	}
	if !out.Succeeded {
		return r, ErrConflict
	}
	r.Metadata.ResourceVersion, err = strconv.ParseInt(out.Header.Revision, 10, 64)
	return r, err
}
