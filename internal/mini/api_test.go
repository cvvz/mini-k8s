package mini

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestServiceSingletonError(t *testing.T) {
	if err := serviceSingletonError("", "web"); err != nil {
		t.Fatalf("first Service rejected: %v", err)
	}
	err := serviceSingletonError("web", "other")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "only one Service is supported") || !strings.Contains(err.Error(), `"web"`) {
		t.Fatalf("unclear second Service error: %v", err)
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
