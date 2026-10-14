package framework

import (
	"bytes"
	"context"
	"os"
	"path/filepath"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

// Object is a manifest document in the same shape users write for kk.
type Object map[string]any

// Container describes the container used by test Pods.
type Container struct {
	Name    string
	Image   string
	Command []string
	Args    []string
	Env     map[string]string
	// Fields are further container fields, such as probes and resources.
	Fields map[string]any
}

// DemoImage is the locally built image; using it avoids registry pulls.
const DemoImage = "mini-k8s:local"

// DemoContainer runs the bundled HTTP demo server on port 8080.
func DemoContainer() Container {
	return Container{Name: "web", Image: DemoImage, Command: []string{"/mini"}, Args: []string{"demo"}}
}

// CrashingContainer exits non-zero immediately (unknown mini role).
func CrashingContainer() Container {
	return Container{Name: "crash", Image: DemoImage, Command: []string{"/mini"}, Args: []string{"e2e-crash-on-purpose"}}
}

// MiniContainer runs `/mini ARGS...` from the demo image, e.g.
// MiniContainer("init", "sleep", "2s") or MiniContainer("idle", "pause").
func MiniContainer(name string, args ...string) Container {
	return Container{Name: name, Image: DemoImage, Command: []string{"/mini"}, Args: args}
}

// WithContainers replaces the containers of a Pod or Deployment template.
func (o Object) WithContainers(cs ...Container) Object {
	o.PodSpec()["containers"] = containerObjects(cs)
	return o
}

// WithInitContainers sets the init containers of a Pod or Deployment template.
func (o Object) WithInitContainers(cs ...Container) Object {
	o.PodSpec()["initContainers"] = containerObjects(cs)
	return o
}

// WithRestartPolicy sets the restart policy of a Pod or Deployment template.
func (o Object) WithRestartPolicy(policy string) Object {
	o.PodSpec()["restartPolicy"] = policy
	return o
}

func containerObjects(cs []Container) []any {
	out := make([]any, len(cs))
	for i, c := range cs {
		out[i] = c.object()
	}
	return out
}

func (c Container) object() map[string]any {
	o := map[string]any{"name": c.Name, "image": c.Image}
	if len(c.Command) > 0 {
		o["command"] = c.Command
	}
	if len(c.Args) > 0 {
		o["args"] = c.Args
	}
	var env []any
	for k, v := range c.Env {
		env = append(env, map[string]any{"name": k, "value": v})
	}
	if env != nil {
		o["env"] = env
	}
	for k, v := range c.Fields {
		o[k] = v
	}
	return o
}

// NewPod builds a Pod manifest.
func NewPod(name string, labels map[string]string, c Container) Object {
	return Object{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "labels": labels},
		"spec":     map[string]any{"containers": []any{c.object()}},
	}
}

// NewDeployment builds a Deployment manifest whose selector equals labels.
func NewDeployment(name string, replicas int, labels map[string]string, c Container) Object {
	return Object{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{
			"replicas": replicas,
			"selector": map[string]any{"matchLabels": labels},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     map[string]any{"containers": []any{c.object()}},
			},
		},
	}
}

// NewService builds a Service manifest for the HTTP gateway.
func NewService(name string, selector map[string]string, targetPort int) Object {
	return Object{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{
			"selector": selector,
			"ports":    []any{map[string]any{"port": 80, "targetPort": targetPort}},
		},
	}
}

// Spec returns the object's spec map for in-place edits.
func (o Object) Spec() map[string]any { s, _ := o["spec"].(map[string]any); return s }

// Meta returns the object's metadata map for in-place edits.
func (o Object) Meta() map[string]any { m, _ := o["metadata"].(map[string]any); return m }

// PodSpec returns the Pod spec of a Pod or of a Deployment template.
func (o Object) PodSpec() map[string]any {
	if o["kind"] == "Pod" {
		return o.Spec()
	}
	tpl, _ := o.Spec()["template"].(map[string]any)
	s, _ := tpl["spec"].(map[string]any)
	return s
}

// WriteManifest renders objects as a multi-document YAML file in a temp dir.
func WriteManifest(objs ...Object) string {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	for _, o := range objs {
		ExpectNoError(enc.Encode(map[string]any(o)))
	}
	ExpectNoError(enc.Close())
	path := filepath.Join(ginkgo.GinkgoT().TempDir(), "manifest.yaml")
	ExpectNoError(os.WriteFile(path, buf.Bytes(), 0o600))
	return path
}

// Apply runs `kk apply -f` in the spec namespace. The namespace's deletion
// cleans everything up.
func (f *Framework) Apply(ctx context.Context, objs ...Object) string {
	return KK(ctx, "apply", "-n", f.Namespace, "-f", WriteManifest(objs...))
}

// ApplyExpectError runs `kk apply -f`, expects failure and returns the output.
func (f *Framework) ApplyExpectError(ctx context.Context, objs ...Object) string {
	out, err := RunKK(ctx, "apply", "-n", f.Namespace, "-f", WriteManifest(objs...))
	gomega.ExpectWithOffset(1, err).To(gomega.HaveOccurred(), "kk apply unexpectedly succeeded:\n%s", out)
	return out
}

// KK runs kk in the spec namespace and fails the spec on error.
func (f *Framework) KK(ctx context.Context, args ...string) string {
	return KK(ctx, append(args, "-n", f.Namespace)...)
}
