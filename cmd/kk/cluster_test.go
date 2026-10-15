package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/integration/framework"
)

func TestTokenCreationAndReuse(t *testing.T) {
	root := t.TempDir()
	first, err := loadToken(root, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadToken(root, true)
	if err != nil || first != second || len(first) != 64 {
		t.Fatalf("token not stable: len=%d err=%v", len(first), err)
	}
	info, err := os.Stat(filepath.Join(root, ".mini-k8s", "token"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("token permission: %v %v", info, err)
	}
	if _, err := loadToken(t.TempDir(), false); err == nil {
		t.Fatal("read-only token lookup created a token")
	}
	if err := os.WriteFile(filepath.Join(root, ".mini-k8s", "token"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(root, true); err == nil {
		t.Fatal("empty existing token must not be replaced silently")
	}
}

func TestDockerEndpointOnlyLocalUnix(t *testing.T) {
	for _, endpoint := range []string{"tcp://127.0.0.1:2375", "ssh://host", "unix://host/path", "unix:relative", "http://localhost", "unix:///tmp/a.sock?remote=true"} {
		if _, err := localSocket(endpoint); err == nil {
			t.Errorf("accepted unsafe endpoint %q", endpoint)
		}
	}
	if got, err := localSocket("unix:///Users/me/.docker/run/docker.sock"); err != nil || got != "/Users/me/.docker/run/docker.sock" {
		t.Fatalf("local socket: %q %v", got, err)
	}
}

func TestClusterDownStopsNodesAndReconcilersFirst(t *testing.T) {
	compose := []string{"docker", "compose", "-f", "compose.yaml", "-f", ".mini-k8s/compose.workers.yaml"}
	for _, volumes := range []bool{false, true} {
		var calls [][]string
		c := cluster{
			run: func(_ context.Context, name string, args ...string) (string, error) {
				calls = append(calls, append([]string{name}, args...))
				return "", nil
			},
			workers: 3,
		}
		if err := c.down(context.Background(), volumes); err != nil {
			t.Fatal(err)
		}
		down := append(append([]string{}, compose...), "down", "--remove-orphans")
		if volumes {
			down = append(down, "--volumes")
		}
		want := [][]string{
			append(append([]string{}, compose...), "stop", "worker-1", "worker-2", "worker-3"),
			down,
		}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("volumes=%v: unsafe down sequence: %#v", volumes, calls)
		}
	}
}

func TestWorkerCountIsRecordedAndValidated(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".mini-k8s"), 0o700); err != nil {
		t.Fatal(err)
	}
	if n, err := loadWorkers(root); err != nil || n != 2 {
		t.Fatalf("default: %d %v", n, err)
	}
	c := cluster{root: root}
	if err := c.setWorkers(4, false); err != nil {
		t.Fatal(err)
	}
	if n, _ := loadWorkers(root); n != 2 {
		t.Fatal("regenerating the override must not change the recorded count")
	}
	if err := c.setWorkers(4, true); err != nil {
		t.Fatal(err)
	}
	if n, err := loadWorkers(root); err != nil || n != 4 {
		t.Fatalf("recorded: %d %v", n, err)
	}
	override, err := os.ReadFile(filepath.Join(root, ".mini-k8s", "compose.workers.yaml"))
	if err != nil || string(override) != workersOverride(4) {
		t.Fatalf("override: %v\n%s", err, override)
	}
	for _, bad := range []string{"0", "11", "-1", "two", ""} {
		if _, err := parseWorkers(bad); err == nil {
			t.Errorf("accepted %q workers", bad)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".mini-k8s", "workers"), []byte("zero\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorkers(root); err == nil || !strings.Contains(err.Error(), ".mini-k8s/workers") {
		t.Fatalf("a corrupt count must be reported: %v", err)
	}
	if err := runCluster(context.Background(), root, args{positional: []string{"down"}, values: map[string][]string{"workers": {"3"}}}); err == nil ||
		err.Error() != "--workers applies to kk cluster up" {
		t.Fatalf("--workers on down: %v", err)
	}
	if err := runCluster(context.Background(), root, args{positional: []string{"up"}, values: map[string][]string{"workers": {"11"}}}); err == nil ||
		!strings.Contains(err.Error(), "must be 1 to 10") {
		t.Fatalf("--workers 11: %v", err)
	}
	for k, name := range map[int]string{3: "worker-3", 10: "worker-10"} {
		if got, ok := workerIndex(name); !ok || got != k {
			t.Errorf("workerIndex(%s) = %d %v", name, got, ok)
		}
	}
	for _, name := range []string{"worker-03", "worker-0", "worker-", "control-plane", "worker-1x"} {
		if _, ok := workerIndex(name); ok {
			t.Errorf("%s is not a worker name", name)
		}
	}
}

func TestWorkersOverride(t *testing.T) {
	type service struct {
		Extends struct {
			File    string `yaml:"file"`
			Service string `yaml:"service"`
		} `yaml:"extends"`
		Hostname    string            `yaml:"hostname"`
		Environment map[string]string `yaml:"environment"`
		Volumes     []string          `yaml:"volumes"`
		Deploy      struct {
			Replicas *int `yaml:"replicas"`
		} `yaml:"deploy"`
	}
	parse := func(n int) (map[string]service, map[string]any) {
		t.Helper()
		var doc struct {
			Services map[string]service `yaml:"services"`
			Volumes  map[string]any     `yaml:"volumes"`
		}
		if err := yaml.Unmarshal([]byte(workersOverride(n)), &doc); err != nil {
			t.Fatalf("%d workers: %v\n%s", n, err, workersOverride(n))
		}
		return doc.Services, doc.Volumes
	}
	if services, volumes := parse(2); len(services) != 0 || len(volumes) != 0 {
		t.Fatalf("two workers is compose.yaml as is: %v %v", services, volumes)
	}
	services, _ := parse(1)
	if r := services["worker-2"].Deploy.Replicas; len(services) != 1 || r == nil || *r != 0 {
		t.Fatalf("one worker scales worker-2 to zero: %+v", services)
	}
	services, volumes := parse(4)
	if len(services) != 2 || len(volumes) != 2 {
		t.Fatalf("four workers add worker-3 and worker-4: %+v %v", services, volumes)
	}
	for _, name := range []string{"worker-3", "worker-4"} {
		s := services[name]
		if s.Extends.File != "compose.yaml" || s.Extends.Service != "worker-1" || s.Hostname != name ||
			s.Environment["MINI_NODE"] != name || !reflect.DeepEqual(s.Volumes, []string{name + "-var:/var"}) {
			t.Errorf("%s: %+v", name, s)
		}
		if _, ok := volumes[name+"-var"]; !ok {
			t.Errorf("volume %s-var is not declared", name)
		}
	}
}

func TestDeleteRemovedNodesOnlyDeletesWorkersAboveTheCount(t *testing.T) {
	c := cluster{client: framework.StartTestServer(t), out: io.Discard, workers: 2}
	ctx := context.Background()
	names := []string{"control-plane", "worker-1", "worker-2", "worker-3", "worker-10", "worker-03", "ghost"}
	for _, name := range names {
		if _, err := client.Nodes(c.client).Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.deleteRemovedNodes(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := client.Nodes(c.client).List(ctx, client.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, n := range list.Items {
		left = append(left, n.Name)
	}
	sort.Strings(left)
	if want := []string{"control-plane", "ghost", "worker-03", "worker-1", "worker-2"}; !reflect.DeepEqual(left, want) {
		t.Fatalf("nodes left: %v", left)
	}
}

// Workers above the count are cordoned first, then drained if Ready; a
// worker without a kubelet is only cordoned, as nothing would confirm the
// evictions.
func TestDrainRemovedWorkers(t *testing.T) {
	var out bytes.Buffer
	c := cluster{client: framework.StartTestServer(t), out: &out, workers: 4}
	ctx := context.Background()
	for _, name := range []string{"worker-1", "worker-2", "worker-3", "worker-4"} {
		n, err := client.Nodes(c.client).Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
		if err != nil {
			t.Fatal(err)
		}
		if name != "worker-3" {
			n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			if _, err := client.Nodes(c.client).UpdateStatus(ctx, n); err != nil {
				t.Fatal(err)
			}
		}
	}
	zero := int64(0)
	for _, node := range []string{"worker-3", "worker-4"} {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "on-" + node}, Spec: corev1.PodSpec{NodeName: node,
			TerminationGracePeriodSeconds: &zero, Containers: []corev1.Container{{Name: "c", Image: "img"}}}}
		if _, err := client.Pods(c.client, "default").Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.drainRemovedWorkers(ctx, 2); err != nil {
		t.Fatal(err)
	}
	for name, cordoned := range map[string]bool{"worker-1": false, "worker-2": false, "worker-3": true, "worker-4": true} {
		n, err := client.Nodes(c.client).Get(ctx, name)
		if err != nil || n.Spec.Unschedulable != cordoned {
			t.Errorf("%s: unschedulable=%v, want %v (%v)", name, n.Spec.Unschedulable, cordoned, err)
		}
	}
	if _, err := client.Pods(c.client, "default").Get(ctx, "on-worker-4"); !client.IsNotFound(err) {
		t.Errorf("the Pod of the drained worker remains: %v", err)
	}
	if _, err := client.Pods(c.client, "default").Get(ctx, "on-worker-3"); err != nil {
		t.Errorf("the Pod of the unready worker is left to the Pod garbage collector: %v", err)
	}
	for _, line := range []string{"Not draining worker-3: it is not Ready\n", "Draining worker-4\n", "node/worker-4 drained\n"} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("output lacks %q:\n%s", line, out.String())
		}
	}
}

func TestDrainWithoutAnAPIGoesOn(t *testing.T) {
	var out bytes.Buffer
	c := cluster{client: client.New("http://127.0.0.1:1", "secret"), out: &out, workers: 3}
	if err := c.drainRemovedWorkers(context.Background(), 3); err != nil || out.Len() != 0 {
		t.Fatalf("not scaling down: %v %q", err, out.String())
	}
	if err := c.drainRemovedWorkers(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "Not draining the workers above 2: the API is unavailable") {
		t.Fatalf("output: %q", out.String())
	}
	got := removedWorkers([]string{"worker-10", "control-plane", "worker-1", "worker-3", "worker-2", "worker-03"}, 2)
	if want := []string{"worker-3", "worker-10"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("removed workers: %v", got)
	}
}

func TestLoadImagesPipesDockerSaveIntoEveryNodeAndRetries(t *testing.T) {
	var pipes [][]string
	failures := 1
	c := cluster{
		out: io.Discard,
		pipe: func(_ context.Context, from, to []string) (string, error) {
			pipes = append(pipes, append(append(append([]string{}, from...), "|"), to...))
			if failures > 0 {
				failures--
				return "", errors.New("containerd is starting")
			}
			return "", nil
		},
	}
	if err := c.loadImages(context.Background(), []string{"app:v1"}, workerNames(2)...); err != nil {
		t.Fatal(err)
	}
	load := func(node string) []string {
		return []string{"docker", "save", "app:v1", "|", "docker", "exec", "-i", "mini-k8s-" + node + "-1", "ctr", "-n", "k8s.io", "images", "import", "-"}
	}
	want := [][]string{load("worker-1"), load("worker-1"), load("worker-2")}
	if !reflect.DeepEqual(pipes, want) {
		t.Fatalf("image load sequence: %#v", pipes)
	}
}

func TestCommandPipeConnectsProducerToConsumer(t *testing.T) {
	pipe := commandPipe(t.TempDir(), os.Environ())
	out, err := pipe(context.Background(), []string{"echo", "hello"}, []string{"tr", "a-z", "A-Z"})
	if err != nil || out != "HELLO\n" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := pipe(context.Background(), []string{"echo", "x"}, []string{"false"}); err == nil {
		t.Fatal("a failing consumer must fail the pipe")
	}
	if _, err := pipe(context.Background(), []string{"false"}, []string{"cat"}); err == nil {
		t.Fatal("a failing producer must fail the pipe")
	}
}

func TestClusterDownDoesNotRemoveWhenStopFails(t *testing.T) {
	calls := 0
	c := cluster{run: func(_ context.Context, name string, args ...string) (string, error) {
		calls++
		return "", errors.New("stop failed")
	}}
	if err := c.down(context.Background(), false); err == nil || !strings.Contains(err.Error(), "stop failed") || calls != 1 {
		t.Fatalf("continued after failed stop: calls=%d err=%v", calls, err)
	}
}

func TestProjectRootRejectsUnrelatedComposeProject(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("name: unrelated\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module other-project\n\ngo 1.23\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if isProject(root) {
		t.Fatal("unrelated Go/Compose project must not be used for cluster operations")
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module mini-k8s\n\ngo 1.23\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if !isProject(root) {
		t.Fatal("mini-k8s root was not recognized")
	}
}

func TestReadinessRequiresAuthenticatedHealthAndAllFreshNodes(t *testing.T) {
	for _, scenario := range []string{"ready", "unauthorized", "stale", "one-worker", "wrong-worker", "no-control-plane"} {
		t.Run(scenario, func(t *testing.T) {
			token := "secret"
			if scenario == "unauthorized" {
				token = "other"
			}
			server := fakeNodeServer(t, token, func() []corev1.Node {
				nodes := []corev1.Node{readyNode("worker-1", time.Now()), readyNode("worker-2", time.Now()), readyNode("control-plane", time.Now())}
				switch scenario {
				case "stale":
					nodes[1] = readyNode("worker-2", time.Now().Add(-time.Minute))
				case "one-worker":
					nodes = append(nodes[:1], nodes[2])
				case "wrong-worker":
					nodes[1].Name = "other"
				case "no-control-plane":
					nodes = nodes[:2]
				}
				return nodes
			})
			c := cluster{client: client.New(server.URL, "secret"), workers: 2}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := c.waitReady(ctx, time.Now().Add(-10*time.Second))
			if scenario == "ready" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("unready cluster did not fail clearly: %v", err)
			}
		})
	}
}

func TestClusterUpRestartsStaticPodsRunningAnOldImage(t *testing.T) {
	var calls []string
	c := cluster{
		out: io.Discard,
		run: func(_ context.Context, name string, args ...string) (string, error) {
			cmd := strings.Join(append([]string{name}, args...), " ")
			calls = append(calls, cmd)
			switch {
			case strings.Contains(cmd, "crictl ps -o json"):
				return `{"containers": [
					{"id": "old", "imageRef": "sha256:1", "image": {"image": "docker.io/library/mini-k8s:local"}, "labels": {"io.kubernetes.pod.name": "kube-apiserver-control-plane"}},
					{"id": "fresh", "imageRef": "sha256:2", "image": {"image": "docker.io/library/mini-k8s:local"}, "labels": {}}]}`, nil
			case strings.Contains(cmd, "inspecti"):
				return "sha256:2\n", nil
			}
			return "", nil
		},
	}
	if err := c.restartStaleSystemPods(context.Background(), "control-plane"); err != nil {
		t.Fatal(err)
	}
	var stops []string
	for _, cmd := range calls {
		if strings.Contains(cmd, "crictl stop") {
			stops = append(stops, cmd)
		}
	}
	if len(stops) != 1 || !strings.HasSuffix(stops[0], " old") {
		t.Fatalf("only the container on the old image restarts: %v", stops)
	}
}

func TestClusterUpAppliesTheAddons(t *testing.T) {
	var out bytes.Buffer
	c := cluster{client: framework.StartTestServer(t), out: &out, addons: filepath.Join("..", "..", "build", "manifests", "addons")}
	ctx := context.Background()
	for range 2 { // cluster up runs on every start
		if err := c.applyAddons(ctx); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := client.Services(c.client, "kube-system").Get(ctx, "kube-dns")
	if err != nil || svc.Spec.ClusterIP != "10.96.0.10" {
		t.Fatalf("kube-dns Service: %v %+v", err, svc)
	}
	for _, name := range []string{"kube-dns", "metrics-server"} {
		if _, err := client.Deployments(c.client, "kube-system").Get(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(out.String(), "deployment.apps/kube-dns unchanged") {
		t.Fatalf("re-applying changes nothing:\n%s", out.String())
	}
}
