package mini

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIRejectsUnauthenticatedRequestsBeforeStorage(t *testing.T) {
	s := httptest.NewServer(APIHandler(nil, "test-token"))
	defer s.Close()
	resp, err := http.Get(s.URL + "/api/Pod")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestCanonicalResourceNames(t *testing.T) {
	for in, want := range map[string]string{"pods": "Pod", "po": "Pod", "deployments": "Deployment", "svc": "Service", "nodes": "Node"} {
		got, err := Kind(in)
		if err != nil || got != want {
			t.Fatalf("%s -> %s (%v)", in, got, err)
		}
	}
	if _, err := Kind("../anything"); err == nil {
		t.Fatal("invalid kind accepted")
	}
}
