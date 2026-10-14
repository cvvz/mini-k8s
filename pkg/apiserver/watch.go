package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"mini-k8s/pkg/storage/etcd3"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/labels"
	"mini-k8s/pkg/fields"
	"mini-k8s/pkg/watch"
	"mini-k8s/pkg/scheme"
)

type watchEvent struct {
	Type   string `json:"type"`
	Object any    `json:"object"`
}

func errExpired() *statusError {
	return newStatus(http.StatusGone, metav1.ReasonExpired, "too old resource version; list again to get a fresh resourceVersion")
}

// serveWatch streams changes as newline-delimited watch events. Without a
// resourceVersion it first sends the current objects as ADDED events.
func (s *Server) serveWatch(w http.ResponseWriter, r *http.Request, info requestInfo, sel labels.Selector, fsel fields.Selector) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, errors.New("streaming is not supported by this connection"))
		return
	}
	q := r.URL.Query()
	timeout := s.cfg.WatchTimeout
	if v := q.Get("timeoutSeconds"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && time.Duration(n)*time.Second < timeout {
			timeout = time.Duration(n) * time.Second
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	var initial []metav1.Object
	var start int64
	if rv := q.Get("resourceVersion"); rv == "" || rv == "0" {
		objs, rev, err := s.list(ctx, info.res, info.namespace)
		if err != nil {
			writeError(w, err)
			return
		}
		for _, obj := range objs {
			if matches(obj, sel, fsel) {
				initial = append(initial, obj)
			}
		}
		start = rev + 1
	} else {
		n, err := strconv.ParseInt(rv, 10, 64)
		if err != nil {
			writeError(w, errBadRequest("invalid resourceVersion %q", rv))
			return
		}
		start = n + 1
	}
	events, errs, err := s.cfg.Store.Watch(ctx, s.listPrefix(info.res, info.namespace), start)
	if errors.Is(err, etcd3.ErrCompacted) {
		writeError(w, errExpired())
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	enc := json.NewEncoder(w)
	send := func(typ string, obj any) bool {
		if err := enc.Encode(watchEvent{Type: typ, Object: obj}); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, obj := range initial {
		if !send(watch.Added, obj) {
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				if err := <-errs; errors.Is(err, etcd3.ErrCompacted) {
					send(watch.Error, errExpired().status)
				}
				return
			}
			typ, obj := s.translate(info.res, ev, sel, fsel)
			if typ != "" && !send(typ, obj) {
				return
			}
		}
	}
}

// translate converts an etcd event to a watch event as seen through the
// selectors: objects entering the selection are ADDED and objects leaving
// it are DELETED.
func (s *Server) translate(res scheme.ResourceInfo, ev etcd3.Event, sel labels.Selector, fsel fields.Selector) (string, metav1.Object) {
	var cur, prev metav1.Object
	if ev.Type == etcd3.Put {
		obj, err := s.decode(res, ev.KV)
		if err != nil {
			return "", nil
		}
		cur = obj
	}
	if ev.Prev != nil {
		obj, err := s.decode(res, *ev.Prev)
		if err != nil {
			return "", nil
		}
		prev = obj
	}
	curMatch := cur != nil && matches(cur, sel, fsel)
	prevMatch := prev != nil && matches(prev, sel, fsel)
	switch {
	case ev.Type == etcd3.Delete:
		if prevMatch {
			prev.GetObjectMeta().ResourceVersion = strconv.FormatInt(ev.KV.ModRevision, 10)
			return watch.Deleted, prev
		}
	case curMatch && prevMatch:
		return watch.Modified, cur
	case curMatch:
		return watch.Added, cur
	case prevMatch:
		return watch.Deleted, cur
	}
	return "", nil
}
