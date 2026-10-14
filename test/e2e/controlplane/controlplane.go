// Package controlplane holds specs for the self-hosted control plane: static
// Pods on the control-plane node, their mirror Pods, and its taint.
package controlplane

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

const node = "control-plane"

var _ = framework.SIGDescribe("cluster-lifecycle", "Self-hosted control plane", func() {
	f := framework.NewDefaultFramework("controlplane")

	mirror := func(ctx SpecContext, component string) (*corev1.Pod, error) {
		return client.Pods(f.Client, "kube-system").Get(ctx, component+"-"+node)
	}

	It("should run the control plane as static Pods mirrored into kube-system", func(ctx SpecContext) {
		cp, err := client.Nodes(f.Client).Get(ctx, node)
		framework.ExpectNoError(err)
		Expect(cp.Labels).To(HaveKey(corev1.LabelNodeRoleControlPlane))
		Expect(cp.Spec.Taints).To(ContainElement(corev1.Taint{Key: corev1.TaintNodeRoleControlPlane, Effect: corev1.TaintEffectNoSchedule}))
		Expect(framework.KK(ctx, "get", "nodes")).To(And(
			MatchRegexp(`control-plane\s+Ready\s+control-plane`), MatchRegexp(`worker-1\s+Ready\s+worker`)))

		for _, component := range framework.ControlPlaneComponents {
			p, err := mirror(ctx, component)
			framework.ExpectNoError(err, component)
			Expect(corev1.IsMirrorPod(p)).To(BeTrue(), component)
			Expect(p.Annotations).To(HaveKeyWithValue(corev1.AnnotationConfigSource, "file"))
			Expect(p.OwnerReferences).To(ConsistOf(HaveField("Kind", "Node")), component)
			Expect(p.OwnerReferences[0].UID).To(Equal(cp.UID))
			Expect(p.Spec.NodeName).To(Equal(node))
			Expect(p.Spec.HostNetwork).To(BeTrue())
			Expect(p.Status.Phase).To(Equal(corev1.PodRunning), component)
			Expect(corev1.IsPodReady(p)).To(BeTrue(), component)
			Expect(p.Status.PodIP).To(Equal(cp.InternalIP()), "hostNetwork Pods have the node's address")
		}
		Expect(framework.KK(ctx, "get", "pods", "-n", "kube-system")).To(MatchRegexp(`kube-apiserver-control-plane\s+1/1\s+Running`))
		Expect(framework.KK(ctx, "logs", "-n", "kube-system", "kube-apiserver-control-plane")).To(ContainSubstring("kube-apiserver listening on :8080"))
		Expect(framework.KK(ctx, "get", "pod", "-n", "kube-system", "kube-apiserver-control-plane", "-o", "yaml")).NotTo(
			ContainSubstring(framework.TestContext.Token), "the token never appears in a manifest or mirror Pod")
	})

	It("should recreate a deleted mirror Pod without restarting the component", func(ctx SpecContext) {
		before, err := mirror(ctx, "kube-scheduler")
		framework.ExpectNoError(err)
		framework.KK(ctx, "delete", "pod", "-n", "kube-system", before.Name)
		var after *corev1.Pod
		Eventually(ctx, func(g Gomega) {
			p, err := mirror(ctx, "kube-scheduler")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(p.UID).NotTo(Equal(before.UID))
			g.Expect(p.Status.ContainerStatuses).To(HaveLen(1))
			after = p
		}).WithTimeout(30 * time.Second).Should(Succeed())
		Expect(after.Status.ContainerStatuses[0].ContainerID).To(Equal(before.Status.ContainerStatuses[0].ContainerID),
			"the static Pod keeps running")
		Expect(after.Status.ContainerStatuses[0].RestartCount).To(Equal(before.Status.ContainerStatuses[0].RestartCount))
	})

	It("should restart a control-plane component that stops", framework.WithDisruptive(), Serial, func(ctx SpecContext) {
		before, err := mirror(ctx, "kube-apiserver")
		framework.ExpectNoError(err)
		id := strings.TrimPrefix(before.Status.ContainerStatuses[0].ContainerID, "containerd://")

		By("stopping the kube-apiserver container behind the kubelet's back")
		_, err = framework.Crictl(ctx, node, "stop", "--timeout", "0", id)
		framework.ExpectNoError(err)
		framework.WaitForClusterReady(ctx, len(framework.Nodes))
		Eventually(ctx, func(g Gomega) {
			p, err := mirror(ctx, "kube-apiserver")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(p.Status.ContainerStatuses[0].RestartCount).To(BeNumerically(">", before.Status.ContainerStatuses[0].RestartCount))
			g.Expect(corev1.IsPodReady(p)).To(BeTrue())
		}).WithTimeout(60 * time.Second).Should(Succeed())
	})

	It("should stop a static Pod whose manifest is removed and start it again when it returns", framework.WithDisruptive(), Serial, func(ctx SpecContext) {
		manifest := filepath.Join(framework.TestContext.RepoRoot, "build", "manifests", "control-plane", "kube-scheduler.yaml")
		aside := filepath.Join(GinkgoT().TempDir(), "kube-scheduler.yaml")
		data, err := os.ReadFile(manifest)
		framework.ExpectNoError(err)
		framework.ExpectNoError(os.WriteFile(aside, data, 0o644))
		DeferCleanup(func() { framework.ExpectNoError(os.WriteFile(manifest, data, 0o644)) })

		By("moving the manifest out of the static Pod directory")
		framework.ExpectNoError(os.Remove(manifest))
		Eventually(ctx, func() bool {
			_, err := mirror(ctx, "kube-scheduler")
			return client.IsNotFound(err)
		}).WithTimeout(60*time.Second).Should(BeTrue(), "the static Pod stops and its mirror Pod goes")
		Eventually(ctx, func() (string, error) {
			return framework.Crictl(ctx, node, "ps", "-q", "--name", "kube-scheduler")
		}).WithTimeout(30 * time.Second).Should(BeEmpty())

		By("putting it back")
		framework.ExpectNoError(os.WriteFile(manifest, data, 0o644))
		Eventually(ctx, func(g Gomega) {
			p, err := mirror(ctx, "kube-scheduler")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(corev1.IsPodReady(p)).To(BeTrue())
		}).WithTimeout(60 * time.Second).Should(Succeed())
	})

	It("should keep Pods without a matching toleration off the control-plane node", framework.WithDisruptive(), Serial, func(ctx SpecContext) {
		cordon := func(unschedulable bool) {
			for _, w := range framework.Workers {
				_, err := client.Nodes(f.Client).Patch(ctx, w, map[string]any{"spec": map[string]any{"unschedulable": unschedulable}})
				framework.ExpectNoError(err)
			}
		}
		By("cordoning the workers, so only the control-plane node remains")
		cordon(true)
		DeferCleanup(func(ctx SpecContext) {
			for _, w := range framework.Workers {
				_, err := client.Nodes(f.Client).Patch(ctx, w, map[string]any{"spec": map[string]any{"unschedulable": false}})
				framework.ExpectNoError(err)
			}
		})

		toleration := []any{map[string]any{"key": corev1.TaintNodeRoleControlPlane, "operator": "Exists", "effect": "NoSchedule"}}
		tolerant := framework.NewPod("tolerant", map[string]string{"app": "tolerant"}, framework.DemoContainer())
		tolerant.Spec()["tolerations"] = toleration
		f.Apply(ctx, framework.NewPod("plain", map[string]string{"app": "plain"}, framework.DemoContainer()), tolerant)

		pending := f.WaitForPod(ctx, "plain", "is unschedulable", func(p *corev1.Pod) bool {
			c := p.Status.Condition(corev1.PodScheduled)
			return c != nil && c.Status == corev1.ConditionFalse
		})
		Expect(pending.Status.Condition(corev1.PodScheduled).Message).To(Equal(
			"0/3 nodes are available: 1 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }, 2 node(s) were unschedulable."))
		running := f.WaitForRunningPods(ctx, map[string]string{"app": "tolerant"}, 1)[0]
		Expect(running.Spec.NodeName).To(Equal(node), "a Pod that tolerates the taint may run on the control-plane node")

		By("uncordoning the workers")
		cordon(false)
		scheduled := f.WaitForRunningPods(ctx, map[string]string{"app": "plain"}, 1)[0]
		Expect(scheduled.Spec.NodeName).To(BeElementOf(framework.Workers))
	})
})
