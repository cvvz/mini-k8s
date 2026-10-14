package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mini-k8s/internal/mini"
)

func TestDeletePodByNamePreservesVersionAndSurfacesConflict(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "delete", true: "conflict"}[conflict], func(t *testing.T) {
			deleted := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/Pod/web-abc" {
					t.Errorf("wrong path: %s", r.URL)
				}
				switch r.Method {
				case http.MethodGet:
					_ = json.NewEncoder(w).Encode(mini.Resource{
						Kind: "Pod", Metadata: mini.Metadata{Name: "web-abc", UID: "pod-uid", ResourceVersion: 123},
					})
				case http.MethodDelete:
					deleted++
					if r.URL.Query().Get("version") != "123" {
						t.Errorf("lost current version: %s", r.URL)
					}
					if conflict {
						http.Error(w, "conflict", http.StatusConflict)
					}
				default:
					t.Errorf("unexpected method: %s", r.Method)
				}
			}))
			defer server.Close()
			var output bytes.Buffer
			a := app{client: mini.NewClient(server.URL, "test-token"), out: &output}
			err := a.run(context.Background(), []string{"delete", "pod", "web-abc"})
			if conflict && !errors.Is(err, mini.ErrConflict) || !conflict && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if deleted != 1 {
				t.Fatalf("expected one version-checked deletion: %d", deleted)
			}
			if !conflict && !strings.Contains(output.String(), "Pod/web-abc deleted") {
				t.Fatalf("missing success output: %q", output.String())
			}
			if conflict && output.Len() != 0 {
				t.Fatal("conflict produced success output")
			}
		})
	}
}
