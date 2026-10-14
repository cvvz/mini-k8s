package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/integration/framework"
)

const webManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 2
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      containers:
        - name: web
          image: mini-k8s:local
          command: ["/mini"]
          args: ["demo"]
`

// kk runs one command line against c and returns its output.
func kk(t *testing.T, c *client.Client, stdin string, argv ...string) (string, error) {
	t.Helper()
	command := findCommand(argv)
	parsed, err := parseArgs(argv, append(append([]flagSpec{}, globalFlags...), commandFlags[command]...))
	if err != nil {
		return "", err
	}
	parsed.positional = parsed.positional[1:]
	var out bytes.Buffer
	err = newApp(c, &out, strings.NewReader(stdin), parsed).run(context.Background(), command, parsed)
	return out.String(), err
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseArgsFlagsAnywhere(t *testing.T) {
	argv := []string{"-n", "team", "get", "pods", "-o=json", "--selector", "app=web", "-w"}
	if findCommand(argv) != "get" {
		t.Fatal("command not found after global flags")
	}
	p, err := parseArgs(argv, append(append([]flagSpec{}, globalFlags...), commandFlags["get"]...))
	if err != nil {
		t.Fatal(err)
	}
	if p.value("namespace", "") != "team" || p.value("output", "") != "json" || p.value("selector", "") != "app=web" || !p.boolean("watch", false) {
		t.Fatalf("parsed: %+v", p)
	}
	if strings.Join(p.positional, " ") != "get pods" {
		t.Fatalf("positional: %v", p.positional)
	}
	p, err = parseArgs([]string{"delete", "pod", "x", "--wait=false"}, commandFlags["delete"])
	if err != nil || p.boolean("wait", true) {
		t.Fatalf("--wait=false: %+v %v", p, err)
	}
	if _, err := parseArgs([]string{"get", "--bogus"}, commandFlags["get"]); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if _, err := parseArgs([]string{"get", "pods", "-o"}, commandFlags["get"]); err == nil {
		t.Fatal("missing flag value accepted")
	}
}

func TestParseTargets(t *testing.T) {
	ts, err := parseTargets([]string{"po,deploy"})
	if err != nil || len(ts) != 2 || ts[0].res.Kind != "Pod" || ts[1].res.Kind != "Deployment" {
		t.Fatalf("%+v %v", ts, err)
	}
	ts, err = parseTargets([]string{"pod/a", "svc/b"})
	if err != nil || ts[0].names[0] != "a" || ts[1].res.Kind != "Service" {
		t.Fatalf("%+v %v", ts, err)
	}
	for _, bad := range [][]string{{}, {"widgets"}, {"pods,svc", "a"}} {
		if _, err := parseTargets(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestApplyCreateConfigureUnchanged(t *testing.T) {
	c := framework.StartTestServer(t)
	path := writeFile(t, webManifest)
	out, err := kk(t, c, "", "apply", "-f", path)
	if err != nil || out != "deployment.apps/web created\n" {
		t.Fatalf("%q %v", out, err)
	}
	if out, _ = kk(t, c, "", "apply", "-f", path); out != "deployment.apps/web unchanged\n" {
		t.Fatalf("second apply: %q", out)
	}
	scaled := writeFile(t, strings.Replace(webManifest, "replicas: 2", "replicas: 3", 1))
	if out, _ = kk(t, c, "", "apply", "-f", scaled); out != "deployment.apps/web configured\n" {
		t.Fatalf("changed apply: %q", out)
	}
	d, err := client.Deployments(c, "default").Get(context.Background(), "web")
	if err != nil || *d.Spec.Replicas != 3 {
		t.Fatalf("replicas not updated: %v", err)
	}
	if out, err = kk(t, c, "", "create", "-f", path); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create of existing object: %q %v", out, err)
	}
	if out, err = kk(t, c, webManifest, "apply", "-f", "-", "-n", "default"); err != nil || out != "deployment.apps/web configured\n" {
		t.Fatalf("stdin apply: %q %v", out, err)
	}
	if d, _ := client.Deployments(c, "default").Get(context.Background(), "web"); *d.Spec.Replicas != 2 {
		t.Fatal("stdin manifest not applied")
	}
}

func TestApplyValidatesWholeBatchFirst(t *testing.T) {
	c := framework.StartTestServer(t)
	bad := webManifest + "---\n" + strings.Replace(strings.Replace(webManifest, "name: web\n", "name: broken\n", 1), "replicas: 2", "replicas: -1", 1)
	_, err := kk(t, c, "", "apply", "-f", writeFile(t, bad))
	if err == nil || !strings.Contains(err.Error(), "no objects were changed") || !strings.Contains(err.Error(), "spec.replicas") {
		t.Fatalf("invalid batch: %v", err)
	}
	if _, err := client.Deployments(c, "default").Get(context.Background(), "web"); !client.IsNotFound(err) {
		t.Fatalf("valid document of an invalid batch was applied: %v", err)
	}
	for _, tc := range []struct{ manifest, want string }{
		{strings.Replace(webManifest, "metadata:\n  name: web", "metadata:\n  name: web\n  uid: x", 1), "server-managed"},
		{webManifest + "status: {}\n", "status is managed"},
		{strings.Replace(webManifest, "kind: Deployment", "kind: Widget", 1), "no matches for kind"},
		{strings.Replace(webManifest, "  replicas: 2", "  replicas: 2\n  suspended: true", 1), "unknown field"},
	} {
		if _, err := kk(t, c, "", "apply", "-f", writeFile(t, tc.manifest)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("want %q, got %v", tc.want, err)
		}
	}
	nsManifest := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: team\n---\n" + strings.Replace(webManifest, "name: web\n", "name: web\n  namespace: team\n", 1)
	out, err := kk(t, c, "", "apply", "-f", writeFile(t, nsManifest))
	if err != nil || !strings.Contains(out, "namespace/team created") || !strings.Contains(out, "deployment.apps/web created") {
		t.Fatalf("namespace and its content in one batch: %q %v", out, err)
	}
	if _, err := kk(t, c, "", "apply", "-n", "other", "-f", writeFile(t, nsManifest)); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("namespace mismatch: %v", err)
	}
}

func TestGetOutputs(t *testing.T) {
	c := framework.StartTestServer(t)
	if out, err := kk(t, c, "", "get", "pods"); err != nil || out != "No resources found in default namespace.\n" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := kk(t, c, "", "apply", "-f", writeFile(t, webManifest)); err != nil {
		t.Fatal(err)
	}
	out, err := kk(t, c, "", "get", "deploy")
	if err != nil || !strings.HasPrefix(out, "NAME") || !strings.Contains(out, "web") || !strings.Contains(out, "0/2") {
		t.Fatalf("table: %q %v", out, err)
	}
	if out, _ = kk(t, c, "", "get", "deployments", "-o", "name"); out != "deployment.apps/web\n" {
		t.Fatalf("name output: %q", out)
	}
	out, _ = kk(t, c, "", "get", "deployment", "web", "-o", "json")
	var d appsv1.Deployment
	if err := json.Unmarshal([]byte(out), &d); err != nil || d.Name != "web" || d.Kind != "Deployment" {
		t.Fatalf("json output: %v %s", err, out)
	}
	if out, _ = kk(t, c, "", "get", "deploy", "-o", "yaml"); !strings.Contains(out, "kind: List") || !strings.Contains(out, "name: web") {
		t.Fatalf("yaml output: %s", out)
	}
	if out, _ = kk(t, c, "", "get", "ns", "-A"); !strings.Contains(out, "default") || !strings.Contains(out, "Active") {
		t.Fatalf("namespaces: %s", out)
	}
	if out, _ = kk(t, c, "", "get", "deploy", "-A", "-o", "wide"); !strings.HasPrefix(out, "NAMESPACE") || !strings.Contains(out, "mini-k8s:local") {
		t.Fatalf("all namespaces wide: %s", out)
	}
	if _, err := kk(t, c, "", "get", "pods", "missing"); !client.IsNotFound(err) {
		t.Fatalf("missing object: %v", err)
	}
	if out, _ := kk(t, c, "", "api-resources"); !strings.Contains(out, "deployments") || !strings.Contains(out, "apps/v1") {
		t.Fatalf("api-resources: %s", out)
	}
}

func TestDeleteScaleCreateNamespaceDescribe(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := context.Background()
	if out, err := kk(t, c, "", "create", "namespace", "team"); err != nil || out != "namespace/team created\n" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := kk(t, c, "", "apply", "-n", "team", "-f", writeFile(t, webManifest)); err != nil {
		t.Fatal(err)
	}
	if out, err := kk(t, c, "", "scale", "deployment/web", "--replicas", "5", "-n", "team"); err != nil || out != "deployment.apps/web scaled\n" {
		t.Fatalf("%q %v", out, err)
	}
	if d, _ := client.Deployments(c, "team").Get(ctx, "web"); *d.Spec.Replicas != 5 {
		t.Fatal("scale not applied")
	}
	if _, err := kk(t, c, "", "scale", "deploy", "web", "--replicas", "-1", "-n", "team"); err == nil {
		t.Fatal("negative replicas accepted")
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Labels: map[string]string{"tier": "x"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}}}
	created, err := client.Pods(c, "team").Create(ctx, pod)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.Events(c, "team").Create(ctx, &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "p1.1"}, Reason: "Testing", Message: "hello event",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "p1", Namespace: "team", UID: created.UID}})
	out, err := kk(t, c, "", "describe", "pod", "p1", "-n", "team")
	if err != nil || !strings.Contains(out, "Name:") || !strings.Contains(out, "tier=x") || !strings.Contains(out, "hello event") || !strings.Contains(out, "Spec:") {
		t.Fatalf("describe: %s %v", out, err)
	}
	if out, err := kk(t, c, "", "delete", "pods", "-l", "tier=x", "-n", "team"); err != nil || out != "pod \"p1\" deleted\n" {
		t.Fatalf("delete by selector: %q %v", out, err)
	}
	if _, err := kk(t, c, "", "delete", "pod", "p1", "-n", "team"); !client.IsNotFound(err) {
		t.Fatalf("delete missing: %v", err)
	}
	if out, err := kk(t, c, "", "delete", "pod", "p1", "-n", "team", "--ignore-not-found"); err != nil || out != "" {
		t.Fatalf("ignore-not-found: %q %v", out, err)
	}
	if _, err := kk(t, c, "", "delete", "pods", "-n", "team"); err == nil {
		t.Fatal("delete without names or selector accepted")
	}
	if _, err := kk(t, c, "", "delete", "node", "worker-1"); err == nil {
		t.Fatal("node deletion accepted")
	}
}

func TestPodStatusReasonAndAges(t *testing.T) {
	p := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}}}
	if PodStatusReason(p) != "Pending" {
		t.Fatal(PodStatusReason(p))
	}
	p.Status.Phase = corev1.PodRunning
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "c", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}, RestartCount: 4}}
	if PodStatusReason(p) != "CrashLoopBackOff" || podRestarts(p) != "4" || podReadyCount(p) != "0/1" {
		t.Fatal(PodStatusReason(p), podRestarts(p), podReadyCount(p))
	}
	now := metav1.Now()
	p.DeletionTimestamp = &now
	if PodStatusReason(p) != "Terminating" {
		t.Fatal(PodStatusReason(p))
	}
	for d, want := range map[time.Duration]string{45 * time.Second: "45s", 5*time.Minute + 3*time.Second: "5m3s", 17 * time.Minute: "17m", 3*time.Hour + 4*time.Minute: "3h4m", 50 * time.Hour: "2d"} {
		if got := humanDuration(d); got != want {
			t.Errorf("%v: %s want %s", d, got, want)
		}
	}
}

// TestExamplesFollowTheREADME replays the README walkthrough: the demo in the
// default namespace, then the NodePort example in a second namespace.
func TestExamplesFollowTheREADME(t *testing.T) {
	c := framework.StartTestServer(t)
	example := func(name string) string { return filepath.Join("..", "..", "examples", name) }
	out, err := kk(t, c, "", "apply", "-f", example("demo.yaml"))
	if err != nil || out != "deployment.apps/web created\nservice/web created\ningress.networking.k8s.io/web created\n" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := kk(t, c, "", "create", "namespace", "team"); err != nil {
		t.Fatal(err)
	}
	if out, err = kk(t, c, "", "apply", "-n", "team", "-f", example("hello.yaml")); err != nil || out != "deployment.apps/hello created\nservice/hello created\n" {
		t.Fatalf("hello.yaml in a second namespace: %q %v", out, err)
	}
	if out, err = kk(t, c, "", "apply", "-n", "team", "-f", example("init-and-sidecar.yaml")); err != nil || !strings.Contains(out, "pod/web-with-sidecar created") {
		t.Fatalf("init-and-sidecar.yaml: %q %v", out, err)
	}
}

func TestYAMLUsesTwoSpaceIndent(t *testing.T) {
	var buf bytes.Buffer
	if err := writeStructured(&buf, map[string]any{"spec": map[string]any{"replicas": 2}}, "yaml"); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "spec:\n  replicas: 2\n" {
		t.Fatalf("yaml output: %q", buf.String())
	}
}

func TestApplyBundleWithExistingNamespaceStaysAllOrNothing(t *testing.T) {
	c := framework.StartTestServer(t)
	ns := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: team\n"
	if _, err := kk(t, c, "", "apply", "-f", writeFile(t, ns)); err != nil {
		t.Fatal(err)
	}
	svc := "apiVersion: v1\nkind: Service\nmetadata:\n  name: web\n  namespace: team\nspec:\n  selector: {app: web}\n  ports: [{port: 80, targetPort: 8080}]\n"
	bad := strings.Replace(strings.Replace(webManifest, "name: web\n", "name: web\n  namespace: team\n", 1), "replicas: 2", "replicas: -3", 1)
	_, err := kk(t, c, "", "apply", "-f", writeFile(t, ns+"---\n"+svc+"---\n"+bad))
	if err == nil || !strings.Contains(err.Error(), "no objects were changed") {
		t.Fatalf("bad bundle: %v", err)
	}
	if _, err := client.Services(c, "team").Get(context.Background(), "web"); !client.IsNotFound(err) {
		t.Fatalf("Service of a rejected bundle was created: %v", err)
	}
}

func TestGetWatchByNameOnlyReportsThatObject(t *testing.T) {
	c := framework.StartTestServer(t)
	pods := client.Pods(c, "default")
	for _, name := range []string{"watched", "other"} {
		if _, err := pods.Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	parsed, _ := parseArgs([]string{"get", "pod", "watched", "-w", "-o", "name"}, append(append([]flagSpec{}, globalFlags...), commandFlags["get"]...))
	parsed.positional = parsed.positional[1:]
	var out safeBuffer
	done := make(chan error, 1)
	go func() { done <- newApp(c, &out, strings.NewReader(""), parsed).run(ctx, "get", parsed) }()
	time.Sleep(300 * time.Millisecond)
	for _, name := range []string{"watched", "other"} {
		if _, err := pods.Patch(context.Background(), name, map[string]any{"metadata": map[string]any{"labels": map[string]any{"touched": "yes"}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "pod/watched\npod/watched\n" {
		t.Fatalf("watch output: %q", got)
	}
}

// safeBuffer is a bytes.Buffer safe for one writer and one reader.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
