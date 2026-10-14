package framework

import (
	"context"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/onsi/gomega"

	"mini-k8s/internal/mini"
)

// Pods returns the Pods whose labels contain all of the given labels, sorted by name.
func (f *Framework) Pods(ctx context.Context, labels map[string]string) ([]mini.Resource, error) {
	all, err := f.Client.List(ctx, "Pod")
	if err != nil {
		return nil, err
	}
	var pods []mini.Resource
	for _, p := range all {
		if mini.Matches(labels, p.Metadata.Labels) {
			pods = append(pods, p)
		}
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Metadata.Name < pods[j].Metadata.Name })
	return pods, nil
}

// WaitForRunningPods waits until exactly count Pods match labels, all are
// Running in the API, and each has exactly one running Docker container.
func (f *Framework) WaitForRunningPods(ctx context.Context, labels map[string]string, count int) []mini.Resource {
	var pods []mini.Resource
	gomega.EventuallyWithOffset(1, ctx, func(g gomega.Gomega) {
		ps, err := f.Pods(ctx, labels)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(ps).To(gomega.HaveLen(count), "Pods matching %v", labels)
		for _, p := range ps {
			g.Expect(p.Status.Phase).To(gomega.Equal("Running"), "Pod %s reason: %s", p.Metadata.Name, p.Status.Reason)
			ids, err := PodContainers(ctx, p.Metadata.UID, true)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(ids).To(gomega.HaveLen(1), "running containers of Pod %s", p.Metadata.Name)
		}
		pods = ps
	}).WithTimeout(TestContext.PodStartTimeout).WithPolling(TestContext.Poll).Should(gomega.Succeed())
	return pods
}

// WaitForPod waits until the named Pod satisfies cond and returns it.
func (f *Framework) WaitForPod(ctx context.Context, name, desc string, cond func(mini.Resource) bool) mini.Resource {
	var pod mini.Resource
	gomega.EventuallyWithOffset(1, ctx, func(g gomega.Gomega) {
		p, err := f.Client.Get(ctx, "Pod", name)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(cond(p)).To(gomega.BeTrue(), "Pod %s %s; status: %+v", name, desc, p.Status)
		pod = p
	}).WithTimeout(TestContext.PodStartTimeout).WithPolling(TestContext.Poll).Should(gomega.Succeed())
	Logf("Pod %s %s", name, desc)
	return pod
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
