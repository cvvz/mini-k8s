// Package etcdtest starts a throwaway etcd for component tests. It uses the
// project's cached bin/etcd (or MINI_ETCD_BIN) and skips tests when neither
// is available.
package etcdtesting

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	once    sync.Once
	url     string
	initErr error
)

// URL returns the client URL of a shared etcd started on first use. Tests
// sharing it must use distinct key prefixes.
func URL(t testing.TB) string {
	t.Helper()
	once.Do(func() { url, initErr = start() })
	if initErr != nil {
		t.Skipf("etcd unavailable: %v", initErr)
	}
	return url
}

func binary() (string, error) {
	if b := os.Getenv("MINI_ETCD_BIN"); b != "" {
		return b, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && strings.Contains(string(data), "module mini-k8s\n") {
			b := filepath.Join(dir, "bin", "etcd")
			if _, err := os.Stat(b); err != nil {
				return "", fmt.Errorf("%s missing (run make prepare-etcd)", b)
			}
			return b, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("repository root not found")
		}
		dir = parent
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func start() (string, error) {
	bin, err := binary()
	if err != nil {
		return "", err
	}
	// Data directories of earlier runs are useless once their etcd is gone.
	if stale, _ := filepath.Glob(filepath.Join(os.TempDir(), "mini-etcd-*")); len(stale) > 0 {
		for _, d := range stale {
			if info, err := os.Stat(d); err == nil && time.Since(info.ModTime()) > time.Hour {
				_ = os.RemoveAll(d)
			}
		}
	}
	dir, err := os.MkdirTemp("", "mini-etcd-")
	if err != nil {
		return "", err
	}
	client, err := freePort()
	if err != nil {
		return "", err
	}
	peer, err := freePort()
	if err != nil {
		return "", err
	}
	clientURL := fmt.Sprintf("http://127.0.0.1:%d", client)
	peerURL := fmt.Sprintf("http://127.0.0.1:%d", peer)
	cmd := exec.Command(bin,
		"--data-dir", dir, "--name", "test",
		"--listen-client-urls", clientURL, "--advertise-client-urls", clientURL,
		"--listen-peer-urls", peerURL, "--initial-advertise-peer-urls", peerURL,
		"--initial-cluster", "test="+peerURL, "--log-level", "error")
	bindLifetime(cmd)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	// The process lives for the duration of the test binary.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		resp, err := http.Get(clientURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return clientURL, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return "", fmt.Errorf("etcd did not become healthy")
}

// Prefix returns a key prefix unique to the test.
func Prefix(t testing.TB) string {
	return fmt.Sprintf("/test/%s/%d", strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano())
}
