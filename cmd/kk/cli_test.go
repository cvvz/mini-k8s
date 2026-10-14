package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mini-k8s/internal/mini"
)

func testDeployment(t *testing.T) mini.Resource {
	t.Helper()
	r, err := readManifests(strings.NewReader(deploymentYAML))
	if err != nil {
		t.Fatal(err)
	}
	r[0].Metadata.UID = "server-uid"
	r[0].Metadata.ResourceVersion = 42
	r[0].Status.Phase = "Running"
	return r[0]
}

func TestApplyPreservesIdentityAndSurfacesConflict(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "update", true: "conflict"}[conflict], func(t *testing.T) {
			old := testDeployment(t)
			updates := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing authentication")
				}
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(old)
					return
				}
				updates++
				if r.Method != http.MethodPut || r.URL.Path != "/api/Deployment/web" {
					t.Errorf("unexpected update: %s %s", r.Method, r.URL)
				}
				var next mini.Resource
				if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
					t.Error(err)
				}
				if next.Metadata.UID != "server-uid" || next.Metadata.ResourceVersion != 42 || next.Status.Phase != "Running" {
					t.Errorf("lost server data: %+v", next)
				}
				var spec mini.DeploymentSpec
				_ = json.Unmarshal(next.Spec, &spec)
				if spec.Replicas == nil || *spec.Replicas != 3 {
					t.Errorf("replicas not updated: %s", next.Spec)
				}
				if conflict {
					http.Error(w, "conflict", http.StatusConflict)
				}
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "app.yaml")
			if err := os.WriteFile(file, []byte(strings.Replace(deploymentYAML, "replicas: 2", "replicas: 3", 1)), 0600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			a := app{client: mini.NewClient(server.URL, "test-token"), out: &output}
			err := a.run(context.Background(), []string{"apply", "-f", file})
			if conflict && !errors.Is(err, mini.ErrConflict) || !conflict && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if updates != 1 {
				t.Fatalf("expected one update without retries; got %d", updates)
			}
		})
	}
}

func TestApplyRejectsImmutableChange(t *testing.T) {
	old := testDeployment(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("immutable manifest caused mutation: %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(old)
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(file, []byte(strings.Replace(deploymentYAML, "mini-k8s:local", "other:latest", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	a := app{client: mini.NewClient(server.URL, ""), out: &bytes.Buffer{}}
	if err := a.run(context.Background(), []string{"apply", "-f", file}); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("expected immutable error; got %v", err)
	}
}

func TestGetAliasesAndJSONShapes(t *testing.T) {
	for _, args := range [][]string{
		{"get", "deploy", "-o", "json"},
		{"get", "deployments", "web", "-o=json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/Deployment/web" {
					_ = json.NewEncoder(w).Encode(testDeployment(t))
				} else if r.URL.Path == "/api/Deployment" {
					_ = json.NewEncoder(w).Encode([]mini.Resource{})
				} else {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
			}))
			defer server.Close()
			var output bytes.Buffer
			a := app{client: mini.NewClient(server.URL, ""), out: &output}
			if err := a.run(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			var value any
			if err := json.Unmarshal(output.Bytes(), &value); err != nil {
				t.Fatal(err)
			}
			if args[2] == "web" {
				if _, ok := value.(map[string]any); !ok {
					t.Fatalf("named get must return object: %s", output.String())
				}
			} else if _, ok := value.([]any); !ok {
				t.Fatalf("list must return array: %s", output.String())
			}
		})
	}
}

func TestScaleValidatesAndPreservesMetadata(t *testing.T) {
	old := testDeployment(t)
	updates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(old)
			return
		}

		updates++
		var next mini.Resource
		if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
			t.Error(err)
		}
		var spec mini.DeploymentSpec
		_ = json.Unmarshal(next.Spec, &spec)
		if next.Metadata.UID != old.Metadata.UID || next.Metadata.ResourceVersion != 42 || spec.Replicas == nil || *spec.Replicas != 0 {
			t.Errorf("bad scale request: %+v", next)
		}
	}))
	defer server.Close()
	a := app{client: mini.NewClient(server.URL, ""), out: &bytes.Buffer{}}
	if err := a.run(context.Background(), []string{"scale", "deployment", "web", "--replicas", "0"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"scale", "deployment", "web"},
		{"scale", "deployment", "web", "--replicas", "-1"},
		{"scale", "deployment", "web", "--replicas=21"},
		{"scale", "pod", "web", "--replicas=2"},
	} {
		if err := a.run(context.Background(), args); err == nil {
			t.Fatalf("accepted invalid arguments: %v", args)
		}
	}
	if updates != 1 {
		t.Fatalf("invalid scale mutated resources: %d updates", updates)
	}
}

func TestCreateDeleteDescribeAndLogs(t *testing.T) {
	for _, action := range []string{"apply", "delete", "describe", "logs"} {
		t.Run(action, func(t *testing.T) {
			resource := testDeployment(t)
			resource.Kind = "Pod"
			resource.APIVersion = "v1"
			resource.Spec = json.RawMessage(`{"containers":[{"name":"web","image":"mini-k8s:local"}]}`)
			mutated := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/Pod/web/logs" {
					_, _ = w.Write([]byte("hello from pod\n"))
					return
				}
				switch r.Method {
				case http.MethodGet:
					if action == "apply" {
						http.Error(w, "missing", http.StatusNotFound)
						return
					}
					_ = json.NewEncoder(w).Encode(resource)
				case http.MethodPost:
					if action != "apply" || r.URL.Path != "/api/Pod" {
						t.Errorf("unexpected create: %s", r.URL)
					}
					mutated = true
					_ = json.NewEncoder(w).Encode(resource)
				case http.MethodDelete:
					if action != "delete" || r.URL.Query().Get("version") != "42" {
						t.Errorf("delete lost concurrency version: %s", r.URL)
					}
					mutated = true
					_, _ = w.Write([]byte("{}"))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "pod.yaml")
			if err := os.WriteFile(file, []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: web}\nspec:\n  containers: [{name: web, image: 'mini-k8s:local'}]\n"), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{action, "-f", file}
			if action == "describe" {
				args = []string{"describe", "pod", "web"}
			} else if action == "logs" {
				args = []string{"logs", "web"}
			}
			var output bytes.Buffer
			a := app{client: mini.NewClient(server.URL, ""), out: &output}
			if err := a.run(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			if (action == "apply" || action == "delete") && !mutated {
				t.Fatal("resource was not mutated")
			}
			if action == "logs" && output.String() != "hello from pod\n" {
				t.Fatalf("logs not streamed: %q", output.String())
			}
			if action == "describe" {
				var described mini.Resource
				if err := json.Unmarshal(output.Bytes(), &described); err != nil || described.Metadata.UID != "server-uid" {
					t.Fatalf("incomplete describe output: %s (%v)", output.String(), err)
				}
			}
		})
	}
}

func TestInvalidManifestBatchMakesNoAPIRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid batch caused API request: %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", 500)
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(file, []byte(deploymentYAML+"---\ninvalid: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a := app{client: mini.NewClient(server.URL, ""), out: &bytes.Buffer{}}
	if err := a.run(context.Background(), []string{"apply", "-f", file}); err == nil {
		t.Fatal("invalid second document was ignored")
	}
}
