package framework

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"gopkg.in/yaml.v3"

	"mini-k8s/internal/mini"
)

// Object is a manifest document in the same shape users write for kk.
type Object map[string]any

// Container describes the single container used by test Pods.
type Container struct {
	Name    string
	Image   string
	Command []string
	Args    []string
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

func (c Container) object() map[string]any {
	o := map[string]any{"name": c.Name, "image": c.Image}
	if len(c.Command) > 0 {
		o["command"] = c.Command
	}
	if len(c.Args) > 0 {
		o["args"] = c.Args
	}
	return o
}

// NewPod builds a standalone Pod manifest.
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

// Kind returns the manifest kind.
func (o Object) Kind() string { s, _ := o["kind"].(string); return s }

// Name returns metadata.name.
func (o Object) Name() string {
	m, _ := o["metadata"].(map[string]any)
	s, _ := m["name"].(string)
	return s
}

// PodLabels returns the labels Pods created from this object carry.
func (o Object) PodLabels() map[string]string {
	switch o.Kind() {
	case "Pod":
		m, _ := o["metadata"].(map[string]any)
		l, _ := m["labels"].(map[string]string)
		return l
	case "Deployment":
		spec, _ := o["spec"].(map[string]any)
		tpl, _ := spec["template"].(map[string]any)
		m, _ := tpl["metadata"].(map[string]any)
		l, _ := m["labels"].(map[string]string)
		return l
	}
	return nil
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

// Apply runs `kk apply -f` and registers cleanup that deletes the objects and
// waits until their Pods and containers are gone.
func (f *Framework) Apply(ctx context.Context, objs ...Object) {
	ginkgo.DeferCleanup(f.DeleteAndWait, objs)
	KK(ctx, "apply", "-f", WriteManifest(objs...))
}

// ApplyExpectError runs `kk apply -f`, expects failure and returns the output.
// Anything that was created before the failure is still cleaned up.
func (f *Framework) ApplyExpectError(ctx context.Context, objs ...Object) string {
	ginkgo.DeferCleanup(f.DeleteAndWait, objs)
	out, err := RunKK(ctx, "apply", "-f", WriteManifest(objs...))
	gomega.ExpectWithOffset(1, err).To(gomega.HaveOccurred(), "kk apply unexpectedly succeeded:\n%s", out)
	return out
}

// DeleteAndWait deletes objects (ignoring NotFound) and waits for their Pods
// to disappear from the API and for their containers to be removed.
func (f *Framework) DeleteAndWait(ctx context.Context, objs []Object) {
	var uids []string
	for _, o := range objs {
		if labels := o.PodLabels(); len(labels) > 0 {
			pods, err := f.Pods(ctx, labels)
			ExpectNoError(err)
			for _, p := range pods {
				uids = append(uids, p.Metadata.UID)
			}
		}
	}
	for i := len(objs) - 1; i >= 0; i-- {
		f.deleteIgnoringNotFound(ctx, objs[i].Kind(), objs[i].Name())
	}
	gomega.Eventually(ctx, func(g gomega.Gomega) {
		pods, err := f.Client.List(ctx, "Pod")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		remaining := map[string]bool{}
		for _, p := range pods {
			remaining[p.Metadata.UID] = true
		}
		for _, uid := range uids {
			g.Expect(remaining[uid]).To(gomega.BeFalse(), "Pod %s still exists", uid)
			ids, err := PodContainers(ctx, uid, false)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(ids).To(gomega.BeEmpty(), "containers of Pod %s still exist", uid)
		}
	}).WithTimeout(TestContext.PodStartTimeout).WithPolling(TestContext.Poll).Should(gomega.Succeed())
}

func (f *Framework) deleteIgnoringNotFound(ctx context.Context, kind, name string) {
	gomega.Eventually(ctx, func() error {
		r, err := f.Client.Get(ctx, kind, name)
		if errors.Is(err, mini.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := f.Client.Delete(ctx, r); err != nil && !errors.Is(err, mini.ErrNotFound) {
			return err
		}
		return nil
	}).WithTimeout(30 * time.Second).WithPolling(TestContext.Poll).Should(gomega.Succeed())
}
