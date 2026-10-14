package framework

import (
	"context"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/labels"
)

// Pods returns the live (not terminating) Pods in the spec namespace whose
// labels contain podLabels, sorted by name.
func (f *Framework) Pods(ctx context.Context, podLabels map[string]string) ([]corev1.Pod, error) {
	list, err := client.Pods(f.Client, f.Namespace).List(ctx, client.ListOptions{LabelSelector: labels.SelectorFromSet(podLabels).String()})
	if err != nil {
		return nil, err
	}
	var pods []corev1.Pod
	for _, p := range list.Items {
		if p.DeletionTimestamp == nil {
			pods = append(pods, p)
		}
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	return pods, nil
}

// ContainerID strips the runtime prefix ("containerd://") from the first
// container's status ID.
func ContainerID(p corev1.Pod) string {
	if len(p.Status.ContainerStatuses) == 0 {
		return ""
	}
	id := p.Status.ContainerStatuses[0].ContainerID
	if i := strings.Index(id, "://"); i >= 0 {
		return id[i+3:]
	}
	return id
}

// WaitForRunningPods waits until exactly count live Pods match podLabels, all
// are Running and Ready, and each runs exactly one instance of each of its containers.
func (f *Framework) WaitForRunningPods(ctx context.Context, podLabels map[string]string, count int) []corev1.Pod {
	var pods []corev1.Pod
	gomega.EventuallyWithOffset(1, ctx, func(g gomega.Gomega) {
		ps, err := f.Pods(ctx, podLabels)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(ps).To(gomega.HaveLen(count), "Pods matching %v", podLabels)
		for _, p := range ps {
			g.Expect(p.Status.Phase).To(gomega.Equal(corev1.PodRunning), "Pod %s: %+v", p.Name, p.Status.ContainerStatuses)
			g.Expect(corev1.IsPodReady(&p)).To(gomega.BeTrue(), "Pod %s not Ready", p.Name)
			ids, err := PodContainers(ctx, p.UID, true)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(ids).To(gomega.HaveLen(len(p.Spec.Containers)), "running containers of Pod %s", p.Name)
		}
		pods = ps
	}).WithTimeout(TestContext.PodStartTimeout).WithPolling(TestContext.Poll).Should(gomega.Succeed())
	return pods
}

// WaitForPod waits until the named Pod satisfies cond and returns it.
func (f *Framework) WaitForPod(ctx context.Context, name, desc string, cond func(*corev1.Pod) bool) *corev1.Pod {
	var pod *corev1.Pod
	gomega.EventuallyWithOffset(1, ctx, func(g gomega.Gomega) {
		p, err := client.Pods(f.Client, f.Namespace).Get(ctx, name)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(cond(p)).To(gomega.BeTrue(), "Pod %s %s; status: %+v", name, desc, p.Status)
		pod = p
	}).WithTimeout(TestContext.PodStartTimeout).WithPolling(TestContext.Poll).Should(gomega.Succeed())
	Logf("Pod %s %s", name, desc)
	return pod
}

// WaitForPodGone waits until the Pod no longer exists and its containers and
// log directory are removed from the nodes.
func (f *Framework) WaitForPodGone(ctx context.Context, pod *corev1.Pod, timeout time.Duration) {
	gomega.EventuallyWithOffset(1, ctx, func(g gomega.Gomega) {
		_, err := client.Pods(f.Client, f.Namespace).Get(ctx, pod.Name)
		g.Expect(client.IsNotFound(err)).To(gomega.BeTrue(), "Pod %s still exists (%v)", pod.Name, err)
		ids, err := PodContainers(ctx, pod.UID, false)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(ids).To(gomega.BeEmpty(), "containers of Pod %s still exist", pod.Name)
		dirs, err := PodLogDirs(ctx, pod.UID)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(dirs).To(gomega.BeEmpty(), "log directories of Pod %s still exist", pod.Name)
	}).WithTimeout(timeout).WithPolling(TestContext.Poll).Should(gomega.Succeed())
}

// Events returns Events in the spec namespace about the named object.
func (f *Framework) Events(ctx context.Context, name string) []corev1.Event {
	list, err := client.Events(f.Client, f.Namespace).List(ctx, client.ListOptions{FieldSelector: "involvedObject.name=" + name})
	ExpectNoError(err)
	return list.Items
}

// WaitForEvent waits until an Event with reason is recorded for the object.
func (f *Framework) WaitForEvent(ctx context.Context, name, reason string) corev1.Event {
	var found corev1.Event
	gomega.EventuallyWithOffset(1, ctx, func(g gomega.Gomega) {
		var reasons []string
		for _, e := range f.Events(ctx, name) {
			reasons = append(reasons, e.Reason)
			if e.Reason == reason {
				found = e
				return
			}
		}
		g.Expect(reasons).To(gomega.ContainElement(reason), "events of %s", name)
	}).WithTimeout(TestContext.PodStartTimeout).WithPolling(TestContext.Poll).Should(gomega.Succeed())
	return found
}

// HTTPGet performs one GET with a short timeout and returns status and body.
func HTTPGet(ctx context.Context, url string) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body), err
}
