// Package apimachinery holds API server contract specs.
package apimachinery

import (
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/util/deepcopy"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/pkg/watch"
	"mini-k8s/test/e2e/framework"
)

func pod(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: framework.DemoImage, Command: []string{"/mini"}, Args: []string{"demo"}}}}}
}

var _ = framework.SIGDescribe("api-machinery", "API server", func() {
	f := framework.NewDefaultFramework("api")

	It("should reject writes based on a stale resourceVersion", func(ctx SpecContext) {
		f.Apply(ctx, framework.NewDeployment("rv", 0, map[string]string{"app": "rv"}, framework.DemoContainer()))
		deps := client.Deployments(f.Client, f.Namespace)
		// Wait for the controller's status write so only this spec changes the object.
		var stale *appsv1.Deployment
		Eventually(ctx, func(g Gomega) {
			d, err := deps.Get(ctx, "rv")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(d.Status.ObservedGeneration).To(Equal(d.Generation))
			stale = d
		}).Should(Succeed())
		fresh := deepcopy.Copy(stale)
		fresh.Spec.Replicas = ptr.To[int32](1)
		_, err := deps.Update(ctx, fresh)
		framework.ExpectNoError(err)

		_, err = deps.Update(ctx, stale)
		Expect(client.IsConflict(err)).To(BeTrue(), "expected a conflict, got %v", err)
		rv := stale.ResourceVersion
		err = deps.Delete(ctx, "rv", &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &rv}})
		Expect(client.IsConflict(err)).To(BeTrue(), "stale delete should conflict, got %v", err)
	})

	DescribeTable("should reject invalid manifests and store nothing",
		func(ctx SpecContext, mutate func(framework.Object), want string) {
			obj := framework.NewDeployment("invalid", 1, map[string]string{"app": "invalid"}, framework.DemoContainer())
			mutate(obj)
			Expect(f.ApplyExpectError(ctx, obj)).To(ContainSubstring(want))
			_, err := client.Deployments(f.Client, f.Namespace).Get(ctx, "invalid")
			Expect(client.IsNotFound(err)).To(BeTrue(), "invalid object must not be stored")
		},
		Entry("unknown field", func(o framework.Object) { o.Spec()["paused"] = true }, "unknown field"),
		Entry("negative replicas", func(o framework.Object) { o.Spec()["replicas"] = -1 }, "spec.replicas"),
		Entry("selector not matching the template", func(o framework.Object) {
			o.Spec()["selector"] = map[string]any{"matchLabels": map[string]any{"app": "other"}}
		}, "does not match"),
		Entry("server-managed uid", func(o framework.Object) { o.Meta()["uid"] = "x" }, "server-managed"),
		Entry("status in a manifest", func(o framework.Object) { o["status"] = map[string]any{} }, "status is managed"),
	)

	It("should reject requests without the bearer token", func(ctx SpecContext) {
		code, _, err := framework.HTTPGet(ctx, framework.TestContext.APIURL+"/api/v1/pods")
		framework.ExpectNoError(err)
		Expect(code).To(Equal(http.StatusUnauthorized))
	})

	It("should stream ADDED, MODIFIED and DELETED watch events", func(ctx SpecContext) {
		pods := client.Pods(f.Client, f.Namespace)
		w, err := pods.Watch(ctx, client.ListOptions{LabelSelector: "watched=yes"})
		framework.ExpectNoError(err)
		defer w.Stop()
		// waitFor consumes events until one of the wanted type arrives.
		waitFor := func(typ string) *corev1.Pod {
			timeout := time.After(30 * time.Second)
			for {
				select {
				case ev, ok := <-w.Events():
					Expect(ok).To(BeTrue(), "watch closed: %v", w.Err())
					if ev.Type == typ {
						return ev.Object
					}
				case <-timeout:
					Fail("timed out waiting for a " + typ + " event")
				}
			}
		}
		_, err = pods.Create(ctx, pod("watched", map[string]string{"watched": "yes"}))
		framework.ExpectNoError(err)
		Expect(waitFor(watch.Added).Name).To(Equal("watched"))
		Expect(waitFor(watch.Modified).Spec.NodeName).NotTo(BeEmpty(), "the scheduler binds the Pod")
		_, err = pods.Patch(ctx, "watched", map[string]any{"metadata": map[string]any{"labels": map[string]any{"watched": "no"}}})
		framework.ExpectNoError(err)
		// Leaving the label selection is reported as DELETED.
		Expect(waitFor(watch.Deleted).Labels["watched"]).To(Equal("no"))
	})

	It("should filter lists by label and field selectors", func(ctx SpecContext) {
		pods := client.Pods(f.Client, f.Namespace)
		for _, name := range []string{"a", "b", "c"} {
			tier := "front"
			if name == "c" {
				tier = "back"
			}
			_, err := pods.Create(ctx, pod(name, map[string]string{"tier": tier}))
			framework.ExpectNoError(err)
		}
		Expect(f.KK(ctx, "get", "pods", "-l", "tier=front", "-o", "name")).To(Equal("pod/a\npod/b\n"))
		Expect(f.KK(ctx, "get", "pods", "-l", "tier in (back)", "-o", "name")).To(Equal("pod/c\n"))
		Expect(f.KK(ctx, "get", "pods", "--field-selector", "metadata.name=b", "-o", "name")).To(Equal("pod/b\n"))
	})

	It("should isolate same-named objects in different namespaces", func(ctx SpecContext) {
		other := f.CreateNamespace(ctx, "other")
		_, err := client.Pods(f.Client, f.Namespace).Create(ctx, pod("twin", map[string]string{"side": "a"}))
		framework.ExpectNoError(err)
		_, err = client.Pods(f.Client, other).Create(ctx, pod("twin", map[string]string{"side": "b"}))
		framework.ExpectNoError(err)
		a, _ := client.Pods(f.Client, f.Namespace).Get(ctx, "twin")
		b, _ := client.Pods(f.Client, other).Get(ctx, "twin")
		Expect(a.UID).NotTo(Equal(b.UID))
		Expect(a.Labels["side"]).To(Equal("a"))
		Expect(b.Labels["side"]).To(Equal("b"))
		Expect(framework.KK(ctx, "get", "pods", "-A")).To(And(ContainSubstring(f.Namespace), ContainSubstring(other)))
	})

	It("should delete everything in a namespace when the namespace is deleted", func(ctx SpecContext) {
		ns := f.CreateNamespace(ctx, "doomed")
		framework.KK(ctx, "apply", "-n", ns, "-f", framework.WriteManifest(
			framework.NewDeployment("web", 2, map[string]string{"app": "web"}, framework.DemoContainer())))
		Eventually(ctx, func() int {
			list, _ := client.Pods(f.Client, ns).List(ctx, client.ListOptions{})
			return len(list.Items)
		}).WithTimeout(framework.TestContext.PodStartTimeout).Should(Equal(2))

		framework.KK(ctx, "delete", "namespace", ns, "--wait=false")
		got, err := client.Namespaces(f.Client).Get(ctx, ns)
		if err == nil {
			Expect(got.Status.Phase).To(Equal(corev1.NamespaceTerminating))
			_, err = client.Pods(f.Client, ns).Create(ctx, pod("late", nil))
			Expect(client.IsForbidden(err)).To(BeTrue(), "creates in a terminating namespace must be refused: %v", err)
		}
		Eventually(ctx, func() bool {
			_, err := client.Namespaces(f.Client).Get(ctx, ns)
			return client.IsNotFound(err)
		}).WithTimeout(2 * time.Minute).Should(BeTrue())
		Eventually(ctx, func() ([]string, error) {
			return framework.PodSandboxes(ctx, "io.kubernetes.pod.namespace="+ns)
		}).WithTimeout(30*time.Second).Should(BeEmpty(), "sandboxes of the deleted namespace must be removed")
	})

	It("should generate unique names from generateName", func(ctx SpecContext) {
		p := pod("", nil)
		p.GenerateName = "gen-"
		first, err := client.Pods(f.Client, f.Namespace).Create(ctx, p)
		framework.ExpectNoError(err)
		second, err := client.Pods(f.Client, f.Namespace).Create(ctx, p)
		framework.ExpectNoError(err)
		Expect(first.Name).To(HavePrefix("gen-"))
		Expect(first.Name).NotTo(Equal(second.Name))
	})

	It("should keep an object with finalizers until they are removed", func(ctx SpecContext) {
		p := pod("held", nil)
		p.Finalizers = []string{"e2e.mini-k8s.io/hold"}
		_, err := client.Pods(f.Client, f.Namespace).Create(ctx, p)
		framework.ExpectNoError(err)
		f.KK(ctx, "delete", "pod", "held", "--wait=false")
		held := f.WaitForPod(ctx, "held", "is marked for deletion", func(p *corev1.Pod) bool { return p.DeletionTimestamp != nil })
		Consistently(ctx, func() error {
			_, err := client.Pods(f.Client, f.Namespace).Get(ctx, "held")
			return err
		}).WithTimeout(3*time.Second).Should(Succeed(), "the finalizer must block deletion")

		_, err = client.Pods(f.Client, f.Namespace).Patch(ctx, "held", map[string]any{"metadata": map[string]any{"finalizers": nil}})
		framework.ExpectNoError(err)
		f.WaitForPodGone(ctx, held, time.Minute)
	})

	It("should record Events for scheduling and container starts", func(ctx SpecContext) {
		labels := map[string]string{"app": "events"}
		f.Apply(ctx, framework.NewPod("events", labels, framework.DemoContainer()))
		f.WaitForRunningPods(ctx, labels, 1)
		scheduled := f.WaitForEvent(ctx, "events", "Scheduled")
		Expect(scheduled.Source.Component).To(Equal("default-scheduler"))
		Expect(f.WaitForEvent(ctx, "events", "Started").Source.Component).To(Equal("kubelet"))
		Expect(f.KK(ctx, "describe", "pod", "events")).To(And(ContainSubstring("Events:"), ContainSubstring("Successfully assigned")))
		Expect(f.KK(ctx, "get", "events")).To(ContainSubstring("pod/events"))
	})
})
