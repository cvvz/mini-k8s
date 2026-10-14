package mini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveCluster(t *testing.T) {
	if os.Getenv("MINI_E2E") != "1" {
		t.Skip("set MINI_E2E=1 after kk cluster up")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(root, ".mini-k8s/token"))
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient("http://127.0.0.1:18000", strings.TrimSpace(string(token)))
	ctx := context.Background()
	cli := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(filepath.Join(root, "bin/kk"), args...)
		cmd.Dir = root
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("kk %v: %v\n%s", args, err, b)
		}
		return string(b)
	}
	wait := func(label string, check func() bool) {
		t.Helper()
		until := time.Now().Add(60 * time.Second)
		for time.Now().Before(until) {
			if check() {
				t.Log(label)
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		pods, _ := c.List(ctx, "Pod")
		t.Fatalf("timed out: %s; pods=%+v", label, pods)
	}
	podsRunning := func(count int) bool {
		ps, err := c.List(ctx, "Pod")
		if err != nil || len(ps) != count {
			return false
		}
		for _, p := range ps {
			if p.Status.Phase != "Running" {
				return false
			}
		}
		cmd := exec.Command("docker", "ps", "-aq", "--filter", "label=mini.cluster=mini-k8s", "--filter", "label=mini.pod")
		containers, err := cmd.Output()
		return err == nil && len(strings.Fields(string(containers))) == count
	}
	wait("two nodes ready", func() bool {
		ns, err := c.List(ctx, "Node")
		return err == nil && len(ns) == 2 && ChooseNode(ns, nil, time.Now()) != ""
	})
	cli("apply", "-f", "examples/demo.yaml")
	wait("two real Pods running", func() bool { return podsRunning(2) })
	ps, err := c.List(ctx, "Pod")
	if err != nil {
		t.Fatal(err)
	}
	if ps[0].Status.NodeName == ps[1].Status.NodeName {
		t.Fatalf("Pods not spread: %+v", ps)
	}
	wait("HTTP service responds", func() bool {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get("http://127.0.0.1:18080/")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode == 200 && strings.Contains(string(b), "hello from")
	})
	if logs := cli("logs", ps[0].Metadata.Name); !strings.Contains(logs, "demo web server started") {
		t.Fatalf("unexpected logs: %s", logs)
	}
	t.Log("kk logs returns actual application logs")
	cli("scale", "deployment", "web", "--replicas", "3")
	wait("scaled to three", func() bool { return podsRunning(3) })
	cli("scale", "deployment", "web", "--replicas", "1")
	wait("scaled to one", func() bool { return podsRunning(1) })
	ps, _ = c.List(ctx, "Pod")
	p := ps[0]
	cmd := exec.Command("docker", "kill", p.Status.ContainerID)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kill: %v %s", err, b)
	}
	wait("kubelet restarts terminated container", func() bool {
		now, err := c.Get(ctx, "Pod", p.Metadata.Name)
		return err == nil && now.Status.Phase == "Running" && now.Status.Restarts > p.Status.Restarts
	})
	cli("delete", "pod", p.Metadata.Name)
	wait("Deployment replaces deleted Pod", func() bool {
		now, err := c.List(ctx, "Pod")
		return err == nil && len(now) == 1 && now[0].Metadata.UID != p.Metadata.UID && now[0].Status.Phase == "Running"
	})
	d, err := c.Get(ctx, "Deployment", "web")
	if err != nil {
		t.Fatal(err)
	}
	old := d
	var spec DeploymentSpec
	if err := json.Unmarshal(d.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	n := 2
	spec.Replicas = &n
	d.Spec, _ = json.Marshal(spec)
	if err := c.Update(ctx, d, false); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, old, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected stale write rejection, got %v", err)
	}
	t.Log("stale resourceVersion rejected")
	cmd = exec.Command("docker", "restart", "mini-k8s-apiserver-1", "mini-k8s-controller-1", "mini-k8s-scheduler-1")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restart: %v %s", err, b)
	}
	wait("control plane restart preserves resources and reconciles", func() bool {
		now, err := c.Get(ctx, "Deployment", "web")
		return err == nil && now.Metadata.UID == d.Metadata.UID && podsRunning(2)
	})
	cli("cluster", "down")
	cmd = exec.Command("docker", "ps", "-aq", "--filter", "label=mini.cluster=mini-k8s", "--filter", "label=mini.pod")
	remaining, err := cmd.Output()
	if err != nil || len(strings.Fields(string(remaining))) != 0 {
		t.Fatalf("cluster down left workload containers: %s (%v)", remaining, err)
	}
	t.Log("cluster down cleans only owned workloads")
	cli("cluster", "up")
	wait("cluster up restores persistent desired state and real containers", func() bool {
		now, err := c.Get(ctx, "Deployment", "web")
		return err == nil && now.Metadata.UID == d.Metadata.UID && podsRunning(2)
	})
	t.Log(cli("get", "pods"))
}
