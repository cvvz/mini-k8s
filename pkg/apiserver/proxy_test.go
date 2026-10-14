package apiserver_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/registry"
	"mini-k8s/pkg/storage/etcd3"
	etcdtesting "mini-k8s/pkg/storage/etcd3/testing"
)

// fakeKubelet records the requests the API server proxies to it.
type fakeKubelet struct {
	mu       sync.Mutex
	requests []string
}

func (k *fakeKubelet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	k.requests = append(k.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	k.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer secret" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/exec/"):
		_ = json.NewEncoder(w).Encode(corev1.PodExecResult{Stdout: []byte("hi\n"), ExitCode: 3})
	case strings.HasPrefix(r.URL.Path, "/containerLogs/"):
		_, _ = w.Write([]byte("log line\n"))
	default:
		http.NotFound(w, r)
	}
}

func (k *fakeKubelet) last() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.requests) == 0 {
		return ""
	}
	return k.requests[len(k.requests)-1]
}

func newServerWithKubelet(t *testing.T) (*client.Client, *fakeKubelet) {
	t.Helper()
	kubelet := &fakeKubelet{}
	ks := httptest.NewServer(kubelet)
	t.Cleanup(ks.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ks.URL, "http://"))
	kp, _ := strconv.Atoi(port)
	store, prefix := etcd3.New(etcdtesting.URL(t)), etcdtesting.Prefix(t)
	s := apiserver.New(apiserver.Config{Strategies: registry.New(store, prefix).Strategies, Store: store,
		Token: "secret", Prefix: prefix, KubeletPort: kp})
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s)
	t.Cleanup(hs.Close)
	c := client.New(hs.URL, "secret")
	ctx := context.Background()
	node, err := client.Nodes(c).Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}})
	if err != nil {
		t.Fatal(err)
	}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "127.0.0.1"}}
	if _, err := client.Nodes(c).UpdateStatus(ctx, node); err != nil {
		t.Fatal(err)
	}
	return c, kubelet
}

func TestExecAndLogsAreProxiedToTheKubelet(t *testing.T) {
	c, kubelet := newServerWithKubelet(t)
	ctx := context.Background()
	pods := client.Pods(c, "default")
	p := pod("multi", nil)
	p.Spec.NodeName = "worker-1"
	p.Spec.InitContainers = []corev1.Container{{Name: "setup", Image: "img"}}
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar", Image: "img"})
	if _, err := pods.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	res, err := c.PodExec(ctx, "default", "multi", client.ExecOptions{Container: "sidecar", Command: []string{"cat", "/etc/hosts"}})
	if err != nil || string(res.Stdout) != "hi\n" || res.ExitCode != 3 {
		t.Fatalf("exec: %+v %v", res, err)
	}
	if got := kubelet.last(); got != "POST /exec/default/multi/sidecar?command=cat&command=%2Fetc%2Fhosts" {
		t.Fatalf("kubelet request: %s", got)
	}

	for name, tc := range map[string]struct {
		o    client.ExecOptions
		want string
	}{
		"no container":    {client.ExecOptions{Command: []string{"true"}}, "a container name must be specified for pod multi, choose one of: [c sidecar] or one of the init containers: [setup]"},
		"unknown":         {client.ExecOptions{Container: "nope", Command: []string{"true"}}, "container nope is not valid for pod multi"},
		"missing command": {client.ExecOptions{Container: "c"}, "you must specify at least 1 command"},
	} {
		if _, err := c.PodExec(ctx, "default", "multi", tc.o); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}

	stream, err := c.PodLogs(ctx, "default", "multi", client.LogOptions{Container: "setup", Previous: true})
	if err != nil {
		t.Fatalf("logs of an init container: %v", err)
	}
	stream.Close()
	if got := kubelet.last(); got != "GET /containerLogs/default/multi/setup?previous=true" {
		t.Fatalf("kubelet request: %s", got)
	}

	current, err := pods.Get(ctx, "multi")
	if err != nil {
		t.Fatal(err)
	}
	current.Status.Phase = corev1.PodFailed
	if _, err := pods.UpdateStatus(ctx, current); err != nil {
		t.Fatal(err)
	}
	_, err = c.PodExec(ctx, "default", "multi", client.ExecOptions{Container: "c", Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "cannot exec into a container in a completed pod; current phase is Failed") {
		t.Fatalf("exec into a finished Pod: %v", err)
	}
}
