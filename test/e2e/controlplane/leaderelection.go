package controlplane

import (
	"os"
	"path/filepath"
	"regexp"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coordinationv1 "mini-k8s/pkg/apis/coordination/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("cluster-lifecycle", "Leader election", func() {
	f := framework.NewDefaultFramework("leader-election")

	lease := func(ctx SpecContext, name string) *coordinationv1.Lease {
		l, err := client.Leases(f.Client, "kube-system").Get(ctx, name)
		framework.ExpectNoError(err)
		return l
	}
	holder := func(ctx SpecContext, name string) string { return ptr.Deref(lease(ctx, name).Spec.HolderIdentity, "") }

	It("should keep the controller-manager and scheduler leases held and renewed", func(ctx SpecContext) {
		for _, name := range []string{"kube-controller-manager", "kube-scheduler"} {
			l := lease(ctx, name)
			Expect(ptr.Deref(l.Spec.HolderIdentity, "")).To(MatchRegexp(`^control-plane_[0-9a-f-]{36}$`), name)
			Expect(ptr.Deref(l.Spec.LeaseDurationSeconds, 0)).To(BeEquivalentTo(15))
			renewed := l.Spec.RenewTime.Time
			Eventually(ctx, func() time.Time { return lease(ctx, name).Spec.RenewTime.Time }).WithTimeout(10*time.Second).
				Should(BeTemporally(">", renewed), "%s renews its lease every 2s", name)
		}
		Expect(framework.KK(ctx, "get", "leases", "-n", "kube-system")).To(And(
			MatchRegexp(`(?m)^kube-controller-manager\s+control-plane_`), MatchRegexp(`(?m)^kube-scheduler\s+control-plane_`)))
	})

	It("should hand the controller-manager lease to a standby replica when the leader stops", framework.WithDisruptive(), Serial, func(ctx SpecContext) {
		before := lease(ctx, "kube-controller-manager")
		leader := ptr.Deref(before.Spec.HolderIdentity, "")
		standby := framework.Object{"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "standby-controller-manager", "namespace": "kube-system"},
			"spec": map[string]any{"nodeName": node, "hostNetwork": true, "terminationGracePeriodSeconds": 5,
				"tolerations": []any{map[string]any{"operator": "Exists"}},
				"containers": []any{map[string]any{"name": "standby", "image": framework.DemoImage, "imagePullPolicy": "Never",
					"command": []any{"/mini", "kube-controller-manager"},
					"env": []any{map[string]any{"name": "MINI_TOKEN_FILE", "value": "/etc/mini-k8s/token"},
						map[string]any{"name": "MINI_API", "value": "http://127.0.0.1:8080"}},
					"volumeMounts": []any{map[string]any{"name": "credentials", "mountPath": "/etc/mini-k8s", "readOnly": true}}}},
				"volumes": []any{map[string]any{"name": "credentials", "hostPath": map[string]any{"path": "/etc/mini-k8s", "type": "Directory"}}}}}
		framework.KK(ctx, "apply", "-f", framework.WriteManifest(standby))
		DeferCleanup(func(ctx SpecContext) {
			_, _ = framework.RunKK(ctx, "delete", "pod", "standby-controller-manager", "-n", "kube-system", "--ignore-not-found")
			By("the restarted static Pod takes the lease back")
			Eventually(ctx, func() string {
				l, err := client.Leases(f.Client, "kube-system").Get(ctx, "kube-controller-manager")
				if err != nil {
					return ""
				}
				return ptr.Deref(l.Spec.HolderIdentity, "")
			}).WithTimeout(time.Minute).Should(MatchRegexp(`^control-plane_`))
			framework.WaitForClusterReady(ctx, len(framework.Nodes))
		})

		By("the standby waits while the leader renews")
		var standbyID string
		Eventually(ctx, func() string {
			out, _ := framework.RunKK(ctx, "logs", "standby-controller-manager", "-n", "kube-system")
			if m := regexp.MustCompile(`acquire leader lease kube-system/kube-controller-manager as (\S+)`).FindStringSubmatch(out); m != nil {
				standbyID = m[1]
			}
			return out
		}).WithTimeout(time.Minute).Should(ContainSubstring("new leader elected: " + leader))
		Expect(standbyID).NotTo(BeEmpty())
		Consistently(ctx, func() string { return holder(ctx, "kube-controller-manager") }).WithTimeout(5 * time.Second).Should(Equal(leader))

		By("stopping the leader (its manifest goes away): it releases the lease and the standby takes over at once")
		manifest := filepath.Join(framework.TestContext.RepoRoot, "build", "manifests", "control-plane", "kube-controller-manager.yaml")
		data, err := os.ReadFile(manifest)
		framework.ExpectNoError(err)
		DeferCleanup(func() { framework.ExpectNoError(os.WriteFile(manifest, data, 0o644)) })
		framework.ExpectNoError(os.Remove(manifest))
		Eventually(ctx, func() string { return holder(ctx, "kube-controller-manager") }).WithTimeout(30 * time.Second).Should(Equal(standbyID))
		Expect(ptr.Deref(lease(ctx, "kube-controller-manager").Spec.LeaseTransitions, 0)).To(BeNumerically(">", ptr.Deref(before.Spec.LeaseTransitions, 0)))

		By("the new leader runs the controllers")
		labels := map[string]string{"app": "after-failover"}
		f.Apply(ctx, framework.NewDeployment("after-failover", 2, labels, framework.MiniContainer("app", "sleep", "1h")))
		f.WaitForRunningPods(ctx, labels, 2)
		Eventually(ctx, func() []string {
			events, err := client.Events(f.Client, "kube-system").List(ctx, client.ListOptions{})
			framework.ExpectNoError(err)
			var messages []string
			for _, e := range events.Items {
				if e.InvolvedObject.Kind == "Lease" && e.Reason == "LeaderElection" {
					messages = append(messages, e.Message)
				}
			}
			return messages
		}).WithTimeout(30 * time.Second).Should(ContainElement(standbyID + " became leader"))

		By("the returning static Pod waits as a standby")
		framework.ExpectNoError(os.WriteFile(manifest, data, 0o644))
		Eventually(ctx, func() (string, error) {
			return framework.RunKK(ctx, "logs", "-n", "kube-system", "kube-controller-manager-"+node)
		}).WithTimeout(time.Minute).Should(ContainSubstring("new leader elected: " + standbyID))
		Expect(holder(ctx, "kube-controller-manager")).To(Equal(standbyID))
	})
})
