// Package scenarios holds the [sig-scenarios] e2e specs. Unlike the
// per-area specs, each one combines many features the way an application
// or an operator uses them: workloads with their configuration, storage,
// networking and permissions, through failures and changes.
package scenarios

import (
	"context"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

// readiness is a container's readiness probe on the demo server.
var readiness = map[string]any{"periodSeconds": 1, "httpGet": map[string]any{"path": "/ready", "port": 8080}}

// claimTemplate is a volumeClaimTemplates entry of the default StorageClass.
func claimTemplate(name string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name}, "spec": map[string]any{
		"accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "64Mi"}}}}
}

// newClaim is a PersistentVolumeClaim of the default StorageClass.
func newClaim(name string) framework.Object {
	c := framework.Object(claimTemplate(name))
	c["apiVersion"], c["kind"] = "v1", "PersistentVolumeClaim"
	return c
}

// newStatefulSet builds a StatefulSet governed by the headless Service of
// the same name, with one claim mounted at /data in every Pod.
func newStatefulSet(name string, replicas int, c framework.Container) framework.Object {
	labels := map[string]string{"app": name}
	obj := framework.ContainerObject(c)
	obj["volumeMounts"] = []any{map[string]any{"name": "data", "mountPath": "/data"}}
	return framework.Object{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]any{"name": name}, "spec": map[string]any{
		"replicas": replicas, "serviceName": name, "selector": map[string]any{"matchLabels": labels},
		"template": map[string]any{"metadata": map[string]any{"labels": labels},
			"spec": map[string]any{"terminationGracePeriodSeconds": 1, "containers": []any{obj}}},
		"volumeClaimTemplates": []any{claimTemplate("data")}}}
}

// headlessService selects the Pods labelled app=name on port 8080.
func headlessService(name string) framework.Object {
	svc := framework.NewService(name, map[string]string{"app": name}, 8080)
	svc.Spec()["clusterIP"] = "None"
	return svc
}

// mini runs `/mini ARGS...` in a Pod of namespace ns.
func mini(ctx context.Context, ns, pod string, args ...string) (string, error) {
	return framework.RunKK(ctx, append([]string{"exec", "-n", ns, pod, "--", "/mini"}, args...)...)
}

// getter makes one request and returns the status code and body.
type getter func(ctx context.Context) (int, string, error)

// whileServing runs action and, until it returns, sends a request through
// get every 100ms; every request must succeed. It returns the bodies.
func whileServing(ctx context.Context, get getter, action func()) []string {
	GinkgoHelper()
	done := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(done)
		action()
	}()
	var bodies []string
	for {
		select {
		case <-done:
			return bodies
		case <-time.After(100 * time.Millisecond):
		}
		code, body, err := get(ctx)
		Expect(err).NotTo(HaveOccurred(), "request %d", len(bodies))
		Expect(code).To(Equal(http.StatusOK), "request %d: %s", len(bodies), body)
		bodies = append(bodies, body)
	}
}

// waitForReplacement waits until the named Pod exists again with a UID
// other than old's, Running and Ready, and returns it.
func waitForReplacement(ctx context.Context, f *framework.Framework, old *corev1.Pod) *corev1.Pod {
	GinkgoHelper()
	var p *corev1.Pod
	Eventually(ctx, func(g Gomega) {
		var err error
		p, err = client.Pods(f.Client, f.Namespace).Get(ctx, old.Name)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(p.UID).NotTo(Equal(old.UID), "still the old Pod")
		g.Expect(corev1.IsPodReady(p)).To(BeTrue(), "Pod %s not Ready: %+v", p.Name, p.Status)
	}).WithTimeout(2 * time.Minute).WithPolling(framework.TestContext.Poll).Should(Succeed())
	return p
}
