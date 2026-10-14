// Package framework provides the shared machinery for mini-k8s end-to-end
// tests, modelled after Kubernetes' test/e2e/framework: a suite-wide test
// context, a per-spec Framework that tracks and cleans up what a spec
// creates, and wait/assert helpers that look at both the API and the nodes' containerd.
package framework

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TestContextType holds suite-wide settings resolved once before any spec runs.
type TestContextType struct {
	RepoRoot   string
	APIURL     string
	GatewayURL string
	Token      string
	KKPath     string

	// ClusterReadyTimeout bounds waiting for the API and worker heartbeats.
	ClusterReadyTimeout time.Duration
	// PodStartTimeout bounds waiting for Pods to reach the expected state.
	PodStartTimeout time.Duration
	// Poll is the interval used by all polling helpers.
	Poll time.Duration
}

// TestContext is populated by Setup and read by every spec.
var TestContext TestContextType

// Setup resolves the project root, credentials and endpoints.
func Setup() error {
	root := os.Getenv("MINI_REPO_ROOT")
	if root == "" {
		var err error
		if root, err = findRepoRoot(); err != nil {
			return err
		}
	}
	token, err := os.ReadFile(filepath.Join(root, ".mini-k8s", "token"))
	if err != nil {
		return fmt.Errorf("read cluster token (run ./bin/kk cluster up first): %w", err)
	}
	TestContext = TestContextType{
		RepoRoot:            root,
		APIURL:              envOr("MINI_API", "http://127.0.0.1:18000"),
		GatewayURL:          envOr("MINI_GATEWAY", "http://127.0.0.1:18080"),
		Token:               strings.TrimSpace(string(token)),
		KKPath:              filepath.Join(root, "bin", "kk"),
		ClusterReadyTimeout: 90 * time.Second,
		PodStartTimeout:     60 * time.Second,
		Poll:                500 * time.Millisecond,
	}
	if _, err := os.Stat(TestContext.KKPath); err != nil {
		return fmt.Errorf("kk binary not found (run make build): %w", err)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(data), "module mini-k8s\n") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("cannot locate the mini-k8s repository root; set MINI_REPO_ROOT")
		}
		dir = parent
	}
}
