// Package token gets service account tokens for Pods' projected volumes,
// like k8s.io/kubernetes/pkg/kubelet/token: the kubelet requests a token
// bound to the Pod through the TokenRequest API and reuses it until 80%
// of its lifetime has passed (then the projected file is rewritten).
package token

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	authenticationv1 "mini-k8s/pkg/apis/authentication/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
)

// Manager caches tokens per Pod, account, audience and lifetime.
type Manager struct {
	client *client.Client
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]*authenticationv1.TokenRequest
}

// NewManager requests tokens with the kubelet's client.
func NewManager(c *client.Client) *Manager {
	return &Manager{client: c, now: time.Now, cache: map[string]*authenticationv1.TokenRequest{}}
}

// requiresRefresh is true once 80% of the token's lifetime has passed.
func (m *Manager) requiresRefresh(tr *authenticationv1.TokenRequest) bool {
	if tr.Spec.ExpirationSeconds == nil {
		return true
	}
	ttl := time.Duration(*tr.Spec.ExpirationSeconds) * time.Second
	issued := tr.Status.ExpirationTimestamp.Add(-ttl)
	return m.now().After(issued.Add(ttl * 8 / 10))
}

// ServiceAccountToken returns a token of the Pod's service account bound
// to the Pod.
func (m *Manager) ServiceAccountToken(pod *corev1.Pod, audience string, expirationSeconds int64) (string, error) {
	key := fmt.Sprintf("%s/%s/%s/%d", pod.UID, pod.Spec.ServiceAccountName, audience, expirationSeconds)
	m.mu.Lock()
	cached := m.cache[key]
	m.mu.Unlock()
	if cached != nil && !m.requiresRefresh(cached) {
		return cached.Status.Token, nil
	}
	req := authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expirationSeconds,
		BoundObjectRef: &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID}}}
	if audience != "" {
		req.Spec.Audiences = []string{audience}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var resp authenticationv1.TokenRequest
	path := fmt.Sprintf("/api/v1/namespaces/%s/serviceaccounts/%s/token", pod.Namespace, pod.Spec.ServiceAccountName)
	if err := m.client.Do(ctx, http.MethodPost, path, nil, "", &req, &resp); err != nil {
		if cached != nil && m.now().Before(cached.Status.ExpirationTimestamp.Time) {
			return cached.Status.Token, nil // still valid; retry the refresh later
		}
		return "", fmt.Errorf("failed to fetch token: %w", err)
	}
	m.mu.Lock()
	m.cache[key] = &resp
	// Drop tokens that expired (their Pods are gone).
	for k, tr := range m.cache {
		if m.now().After(tr.Status.ExpirationTimestamp.Time) {
			delete(m.cache, k)
		}
	}
	m.mu.Unlock()
	return resp.Status.Token, nil
}
