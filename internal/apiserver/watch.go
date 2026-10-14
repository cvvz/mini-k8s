package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"mini-k8s/internal/api"
	"mini-k8s/internal/storage"
)

type watchEvent struct {
	Type   string `json:"type"`
	Object any    `json:"object"`
}

func errExpired() *statusError {
	return newStatus(http.StatusGone, api.ReasonExpired, "too old resource version; list again to get a fresh resourceVersion")
}

// serveWatch streams changes as newline-delimited watch events. Without a
// resourceVersion it first sends the current objects as ADDED events.
func (s *Server) serveWatch(w http.ResponseWriter, r *http.Request, info requestInfo, sel api.Selector, fsel api.FieldSelector) {
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

	var initial []api.Object
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
	if errors.Is(err, storage.ErrCompacted) {
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
		if !send(api.EventAdded, obj) {
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				if err := <-errs; errors.Is(err, storage.ErrCompacted) {
					send(api.EventError, errExpired().status)
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
func (s *Server) translate(res api.ResourceInfo, ev storage.Event, sel api.Selector, fsel api.FieldSelector) (string, api.Object) {
	var cur, prev api.Object
	if ev.Type == storage.Put {
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
	case ev.Type == storage.Delete:
		if prevMatch {
			prev.GetObjectMeta().ResourceVersion = strconv.FormatInt(ev.KV.ModRevision, 10)
			return api.EventDeleted, prev
		}
	case curMatch && prevMatch:
		return api.EventModified, cur
	case curMatch:
		return api.EventAdded, cur
	case prevMatch:
		return api.EventDeleted, cur
	}
	return "", nil
}
