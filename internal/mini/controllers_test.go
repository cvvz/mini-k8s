package mini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestSchedulerAssignsTwoPodsAcrossTwoNodes(t *testing.T) {
	var mu sync.Mutex
	nodes := []Resource{
		{Metadata: Metadata{Name: "worker-1"}, Status: Status{Phase: "Ready", Heartbeat: time.Now()}},
		{Metadata: Metadata{Name: "worker-2"}, Status: Status{Phase: "Ready", Heartbeat: time.Now()}},
	}
	pods := []Resource{{Kind: "Pod", Metadata: Metadata{Name: "a"}}, {Kind: "Pod", Metadata: Metadata{Name: "b"}}}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/Node":
			_ = json.NewEncoder(w).Encode(nodes)
		case r.URL.Path == "/api/Pod":
			_ = json.NewEncoder(w).Encode(pods)
		case r.Method == "PUT":
			var p Resource
			_ = json.NewDecoder(r.Body).Decode(&p)
			for i := range pods {
				if pods[i].Metadata.Name == p.Metadata.Name {
					pods[i] = p
				}
			}
			_ = json.NewEncoder(w).Encode(p)
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	if err := Schedule(context.Background(), NewClient(s.URL, "t")); err != nil {
		t.Fatal(err)
	}
	if pods[0].Status.NodeName != "worker-1" || pods[1].Status.NodeName != "worker-2" {
		t.Fatalf("unbalanced: %#v", pods)
	}
}

func TestProxyUnavailableWithoutBackends(t *testing.T) {
	p := NewProxy()
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 503 {
		t.Fatalf("want 503 got %d", w.Code)
	}
}

func TestProxyRoundRobinPreservesPath(t *testing.T) {
	makeBackend := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, name+r.URL.RequestURI()) }))
	}
	a, b := makeBackend("a"), makeBackend("b")
	defer a.Close()
	defer b.Close()
	p := NewProxy()
	p.setTargets([]string{a.URL, b.URL}, nil)
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest("GET", "/hello?q=1", nil))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		got[w.Body.String()] = true
	}
	if !got["a/hello?q=1"] || !got["b/hello?q=1"] {
		t.Fatal(got)
	}
}
