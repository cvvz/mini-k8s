// Package framework runs an in-process API server on a throwaway etcd key
// prefix for component tests of clients, informers and controllers.
package framework

import (
	"context"
	"net/http/httptest"
	"testing"

	"mini-k8s/pkg/apiserver"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/registry"
	"mini-k8s/pkg/storage/etcd3"
	etcdtesting "mini-k8s/pkg/storage/etcd3/testing"
)

// Token is the bearer token accepted by test servers.
const Token = "test-token"

// StartTestServer starts an API server and returns a client for it. Tests are skipped
// when no etcd binary is available.
func StartTestServer(t testing.TB) *client.Client {
	t.Helper()
	s := apiserver.New(apiserver.Config{Strategies: registry.Strategies(), Store: etcd3.New(etcdtesting.URL(t)), Token: Token, Prefix: etcdtesting.Prefix(t)})
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s)
	t.Cleanup(hs.Close)
	return client.New(hs.URL, Token)
}
