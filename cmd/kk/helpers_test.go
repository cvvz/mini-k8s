package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

// readyNode returns a Node whose Ready heartbeat happened at beat.
func readyNode(name string, beat time.Time) corev1.Node {
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: metav1.NewTime(beat)}}
	return n
}

// fakeNodeServer serves /healthz and the Node list, requiring token.
func fakeNodeServer(t *testing.T, token string, nodes func() []corev1.Node) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status"}, Reason: metav1.ReasonUnauthorized, Code: 401})
			return
		}
		switch r.URL.Path {
		case "/healthz":
			_, _ = w.Write([]byte("ok"))
		case "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(metav1.List[corev1.Node]{Items: nodes()})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}
