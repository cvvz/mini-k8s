// Package testenv runs an in-process API server on a throwaway etcd key
// prefix for component tests of clients, informers and controllers.
package testenv

import (
	"context"
	"net/http/httptest"
	"testing"

	"mini-k8s/internal/apiserver"
	"mini-k8s/internal/client"
	"mini-k8s/internal/storage"
	"mini-k8s/internal/storage/etcdtest"
)

// Token is the bearer token accepted by test servers.
const Token = "test-token"

// New starts an API server and returns a client for it. Tests are skipped
// when no etcd binary is available.
func New(t testing.TB) *client.Client {
	t.Helper()
	s := apiserver.New(apiserver.Config{Store: storage.New(etcdtest.URL(t)), Token: Token, Prefix: etcdtest.Prefix(t)})
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s)
	t.Cleanup(hs.Close)
	return client.New(hs.URL, Token)
}
