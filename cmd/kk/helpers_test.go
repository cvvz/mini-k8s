package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mini-k8s/internal/api"
)

// readyNode returns a Node whose Ready heartbeat happened at beat.
func readyNode(name string, beat time.Time) api.Node {
	n := api.Node{ObjectMeta: api.ObjectMeta{Name: name}}
	n.Status.Conditions = []api.NodeCondition{{Type: api.NodeReady, Status: api.ConditionTrue, LastHeartbeatTime: api.NewTime(beat)}}
	return n
}

// fakeNodeServer serves /healthz and the Node list, requiring token.
func fakeNodeServer(t *testing.T, token string, nodes func() []api.Node) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(api.Status{TypeMeta: api.TypeMeta{Kind: "Status"}, Reason: api.ReasonUnauthorized, Code: 401})
			return
		}
		switch r.URL.Path {
		case "/healthz":
			_, _ = w.Write([]byte("ok"))
		case "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(api.List[api.Node]{Items: nodes()})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}
