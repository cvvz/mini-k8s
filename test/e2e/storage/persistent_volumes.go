package storage

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/volume/localpath"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("storage", "PersistentVolumes", func() {
	f := framework.NewDefaultFramework("pv")

	claimObj := func(name string, spec map[string]any) framework.Object {
		s := map[string]any{"accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "64Mi"}}}
		for k, v := range spec {
			s[k] = v
		}
		return framework.Object{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": name}, "spec": s}
	}
	// demoWith runs the demo server with a claim mounted at /data.
	demoWith := func(name, claim string) framework.Object {
		c := framework.DemoContainer()
		c.Fields = map[string]any{"volumeMounts": []any{mount("data", "/data")}}
		pod := framework.NewPod(name, app(name), c)
		pod.Spec()["volumes"] = []any{map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": claim}}}
		return pod
	}
	claimOf := func(ctx SpecContext, name string) *corev1.PersistentVolumeClaim {
		GinkgoHelper()
		pvc, err := client.PersistentVolumeClaims(f.Client, f.Namespace).Get(ctx, name)
		framework.ExpectNoError(err)
		return pvc
	}
	nodeDirs := func(ctx SpecContext, node string) (string, error) {
		return framework.Docker(ctx, "exec", framework.ComponentContainer(node), "sh", "-c", "ls "+localpath.DefaultBaseDir+" 2>/dev/null || true")
	}

	It("should provision a local volume for the first consumer and keep its data for the next Pod", func(ctx SpecContext) {
		Expect(framework.KK(ctx, "get", "storageclass")).To(MatchRegexp(
			`(?m)^standard \(default\)\s+mini-k8s.io/local-path\s+Delete\s+WaitForFirstConsumer\s+false\s+`))
		f.Apply(ctx, claimObj("data", nil))
		pvc := claimOf(ctx, "data")
		Expect(corev1.ClaimClass(pvc)).To(Equal("standard"), "the default class")
		f.WaitForEvent(ctx, "data", "WaitForFirstConsumer")
		Expect(f.KK(ctx, "get", "pvc", "data")).To(MatchRegexp(`(?m)^data\s+Pending\s+standard\s+<unset>\s+`))

		By("starting the first consumer, which gets a volume on its node")
		f.Apply(ctx, demoWith("writer", "data"))
		writer := f.WaitForRunningPods(ctx, app("writer"), 1)[0]
		pvc = claimOf(ctx, "data")
		Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound))
		Expect(pvc.Annotations).To(HaveKeyWithValue(corev1.AnnSelectedNode, writer.Spec.NodeName))
		pvName := "pvc-" + pvc.UID
		Expect(pvc.Spec.VolumeName).To(Equal(pvName))
		pv, err := client.PersistentVolumes(f.Client).Get(ctx, pvName)
		framework.ExpectNoError(err)
		Expect(pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Values).To(Equal([]string{writer.Spec.NodeName}))
		Expect(framework.KK(ctx, "get", "pv", pvName)).To(MatchRegexp(
			`(?m)^` + pvName + `\s+64Mi\s+RWO\s+Delete\s+Bound\s+` + f.Namespace + `/data\s+standard\s+<unset>\s+`))
		Expect(f.KK(ctx, "get", "pvc", "data")).To(MatchRegexp(`(?m)^data\s+Bound\s+` + pvName + `\s+64Mi\s+RWO\s+standard\s+`))
		Expect(f.Events(ctx, "data")).To(ContainElements(HaveField("Reason", "ExternalProvisioning"),
			HaveField("Message", "Successfully provisioned volume "+pvName)))
		_, err = execIn(ctx, f, "writer", "/mini", "write", "/data/greeting", "hello")
		framework.ExpectNoError(err)
		dir := pvName + "_" + f.Namespace + "_data"
		Expect(nodeDirs(ctx, writer.Spec.NodeName)).To(ContainSubstring(dir))

		By("replacing the Pod: the next one follows the volume to its node and reads the data")
		framework.ExpectNoError(client.Pods(f.Client, f.Namespace).Delete(ctx, "writer", nil))
		f.WaitForPodGone(ctx, &writer, time.Minute)
		f.Apply(ctx, demoWith("reader", "data"))
		reader := f.WaitForRunningPods(ctx, app("reader"), 1)[0]
		Expect(reader.Spec.NodeName).To(Equal(writer.Spec.NodeName))
		Expect(execIn(ctx, f, "reader", "/mini", "cat", "/data/greeting")).To(Equal("hello\n"))
		Expect(f.KK(ctx, "describe", "pvc", "data")).To(MatchRegexp(`Used By:\s+reader`))

		By("deleting the claim while the Pod uses it, then the Pod")
		f.KK(ctx, "delete", "pvc", "data", "--wait=false")
		Expect(f.KK(ctx, "get", "pvc", "data")).To(MatchRegexp(`(?m)^data\s+Terminating\s+`))
		framework.ExpectNoError(client.Pods(f.Client, f.Namespace).Delete(ctx, "reader", nil))
		Eventually(ctx, func() bool {
			_, err := client.PersistentVolumeClaims(f.Client, f.Namespace).Get(ctx, "data")
			return client.IsNotFound(err)
		}).WithTimeout(time.Minute).Should(BeTrue(), "the claim goes with its last Pod")
		Eventually(ctx, func() bool {
			_, err := client.PersistentVolumes(f.Client).Get(ctx, pvName)
			return client.IsNotFound(err)
		}).WithTimeout(time.Minute).Should(BeTrue(), "the Delete policy removes the volume")
		Expect(nodeDirs(ctx, writer.Spec.NodeName)).NotTo(ContainSubstring(dir), "and its directory on the node")
		_, err = client.Pods(f.Client, "local-path-storage").Get(ctx, localpath.HelperPodName(pvName))
		Expect(client.IsNotFound(err)).To(BeTrue(), "the helper Pod is cleaned up")
	})

	It("should bind a static volume, honour volumeName and keep a Retain volume when released", func(ctx SpecContext) {
		name := "e2e-" + f.Namespace
		pv := framework.Object{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": map[string]any{"name": name, "labels": map[string]any{"e2e": f.Namespace}},
			"spec": map[string]any{"capacity": map[string]any{"storage": "1Gi"}, "accessModes": []any{"ReadWriteOnce", "ReadOnlyMany"},
				"hostPath": map[string]any{"path": "/var/lib/mini-e2e-pv/" + f.Namespace, "type": "DirectoryOrCreate"}}}
		f.Apply(ctx, pv)
		DeferCleanup(func(ctx SpecContext) {
			if err := client.PersistentVolumes(f.Client).Delete(ctx, name, nil); err != nil && !client.IsNotFound(err) {
				framework.ExpectNoError(err)
			}
		})
		Eventually(ctx, func() (string, error) { return framework.RunKK(ctx, "get", "pv", name) }).
			Should(MatchRegexp(`(?m)^` + name + `\s+1Gi\s+RWO,ROX\s+Retain\s+Available\s+`))

		By("claiming it by name without a class")
		f.Apply(ctx, claimObj("static", map[string]any{"storageClassName": "", "volumeName": name}))
		Eventually(ctx, func() string { return claimOf(ctx, "static").Status.Phase }).WithTimeout(30 * time.Second).Should(Equal(corev1.ClaimBound))
		Expect(f.KK(ctx, "get", "pvc", "static")).To(MatchRegexp(`(?m)^static\s+Bound\s+` + name + `\s+1Gi\s+RWO,ROX\s+<unset>\s+`))
		f.Apply(ctx, demoWith("user", "static"))
		f.WaitForRunningPods(ctx, app("user"), 1)
		_, err := execIn(ctx, f, "user", "/mini", "write", "/data/kept", "still here")
		framework.ExpectNoError(err)

		By("releasing it: a Retain volume stays, with its claim recorded")
		f.KK(ctx, "delete", "pod", "user")
		f.KK(ctx, "delete", "pvc", "static")
		Eventually(ctx, func() (string, error) { return framework.RunKK(ctx, "get", "pv", name) }).WithTimeout(time.Minute).
			Should(MatchRegexp(`(?m)^` + name + `\s+1Gi\s+RWO,ROX\s+Retain\s+Released\s+` + f.Namespace + `/static\s+`))
	})

	It("should report missing claims, classes and invalid claims", func(ctx SpecContext) {
		f.Apply(ctx, demoWith("waiting", "later"))
		f.WaitForPod(ctx, "waiting", "is unschedulable", func(p *corev1.Pod) bool {
			c := p.Status.Condition(corev1.PodScheduled)
			return c != nil && c.Message == `0/3 nodes are available: persistentvolumeclaim "later" not found.`
		})
		f.Apply(ctx, claimObj("later", nil))
		f.WaitForRunningPods(ctx, app("waiting"), 1)

		f.Apply(ctx, claimObj("classless", map[string]any{"storageClassName": "nonexistent"}))
		f.WaitForEvent(ctx, "classless", "ProvisioningFailed")
		Expect(f.Events(ctx, "classless")).To(ContainElement(HaveField("Message", `storageclass.storage.k8s.io "nonexistent" not found`)))

		bad := claimObj("bad", nil)
		delete(bad.Spec(), "accessModes")
		Expect(f.ApplyExpectError(ctx, bad)).To(ContainSubstring("spec.accessModes: Required value: at least 1 access mode is required"))
		bound := claimOf(ctx, "later")
		changed := claimObj("later", map[string]any{"accessModes": []any{"ReadWriteMany"}, "volumeName": bound.Spec.VolumeName})
		Expect(f.ApplyExpectError(ctx, changed)).To(ContainSubstring(
			"spec: Forbidden: spec is immutable after creation except resources.requests and volumeAttributesClassName for bound claims"))
	})
})
