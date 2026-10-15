package storage

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("storage", "Downward API and projected volumes", func() {
	f := framework.NewDefaultFramework("downward")

	field := func(path string) map[string]any { return map[string]any{"fieldRef": map[string]any{"fieldPath": path}} }
	resource := func(container, res, divisor string) map[string]any {
		r := map[string]any{"resource": res, "divisor": divisor}
		if container != "" {
			r["containerName"] = container
		}
		return map[string]any{"resourceFieldRef": r}
	}
	resources := map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "32Mi"},
		"limits": map[string]any{"cpu": "250m", "memory": "64Mi"}}

	It("should expose Pod fields and container resources as environment variables", func(ctx SpecContext) {
		pod := newPod("fields", sleeper(map[string]any{"resources": resources, "env": []any{
			map[string]any{"name": "POD_NAME", "valueFrom": field("metadata.name")},
			map[string]any{"name": "POD_NAMESPACE", "valueFrom": field("metadata.namespace")},
			map[string]any{"name": "POD_UID", "valueFrom": field("metadata.uid")},
			map[string]any{"name": "POD_IP", "valueFrom": field("status.podIP")},
			map[string]any{"name": "HOST_IP", "valueFrom": field("status.hostIP")},
			map[string]any{"name": "NODE_NAME", "valueFrom": field("spec.nodeName")},
			map[string]any{"name": "TIER", "valueFrom": field("metadata.labels['tier']")},
			map[string]any{"name": "CPU_LIMIT", "valueFrom": resource("", "limits.cpu", "1")},
			map[string]any{"name": "CPU_REQUEST_M", "valueFrom": resource("", "requests.cpu", "1m")},
			map[string]any{"name": "MEMORY_LIMIT_MI", "valueFrom": resource("", "limits.memory", "1Mi")},
		}}))
		pod.Meta()["labels"] = map[string]any{"app": "fields", "tier": "backend"}
		f.Apply(ctx, pod)
		p := f.WaitForRunningPods(ctx, app("fields"), 1)[0]
		out, err := execIn(ctx, f, "fields", "/mini", "env")
		framework.ExpectNoError(err)
		Expect(lines(out)).To(ContainElements("POD_NAME=fields", "POD_NAMESPACE="+f.Namespace, "POD_UID="+p.UID,
			"POD_IP="+p.Status.PodIP, "HOST_IP="+p.Status.HostIP, "NODE_NAME="+p.Spec.NodeName, "TIER=backend",
			"CPU_LIMIT=1", "CPU_REQUEST_M=100", "MEMORY_LIMIT_MI=64"))
	})

	It("should write labels, annotations and resources into a downward API volume and follow label changes", func(ctx SpecContext) {
		pod := newPod("podinfo", sleeper(map[string]any{"resources": resources, "volumeMounts": []any{mount("podinfo", "/etc/podinfo")}}),
			map[string]any{"name": "podinfo", "downwardAPI": map[string]any{"items": []any{
				map[string]any{"path": "labels", "fieldRef": map[string]any{"fieldPath": "metadata.labels"}},
				map[string]any{"path": "annotations", "fieldRef": map[string]any{"fieldPath": "metadata.annotations"}},
				map[string]any{"path": "cpu_limit", "resourceFieldRef": map[string]any{"containerName": "c", "resource": "limits.cpu", "divisor": "1m"}},
			}}})
		pod.Meta()["labels"] = map[string]any{"app": "podinfo", "tier": "x"}
		pod.Meta()["annotations"] = map[string]any{"note": `say "hi"`}
		f.Apply(ctx, pod)
		f.WaitForRunningPods(ctx, app("podinfo"), 1)
		Expect(execIn(ctx, f, "podinfo", "/mini", "cat", "/etc/podinfo/labels")).To(Equal("app=\"podinfo\"\ntier=\"x\""))
		Expect(execIn(ctx, f, "podinfo", "/mini", "cat", "/etc/podinfo/annotations")).To(Equal(`note="say \"hi\""`))
		Expect(execIn(ctx, f, "podinfo", "/mini", "cat", "/etc/podinfo/cpu_limit")).To(Equal("250"))

		By("relabelling the Pod")
		f.KK(ctx, "label", "pod", "podinfo", "tier=y", "--overwrite")
		Eventually(ctx, func() (string, error) { return execIn(ctx, f, "podinfo", "/mini", "cat", "/etc/podinfo/labels") }).
			WithTimeout(30 * time.Second).Should(Equal("app=\"podinfo\"\ntier=\"y\""))
	})

	It("should combine a ConfigMap, a Secret and Pod fields in one projected volume", func(ctx SpecContext) {
		f.Apply(ctx, configMap("settings", map[string]any{"mode": "fast", "unused": "x"}), secret("token", map[string]any{"token": "s3cr3t"}))
		f.Apply(ctx, newPod("projected", sleeper(map[string]any{"volumeMounts": []any{mount("all", "/etc/all")}}),
			map[string]any{"name": "all", "projected": map[string]any{"sources": []any{
				map[string]any{"configMap": map[string]any{"name": "settings", "items": []any{map[string]any{"key": "mode", "path": "config/mode"}}}},
				map[string]any{"secret": map[string]any{"name": "token"}},
				map[string]any{"downwardAPI": map[string]any{"items": []any{
					map[string]any{"path": "namespace", "fieldRef": map[string]any{"fieldPath": "metadata.namespace"}}}}},
			}}}))
		f.WaitForRunningPods(ctx, app("projected"), 1)
		listing, err := execIn(ctx, f, "projected", "/mini", "ls", "/etc/all")
		framework.ExpectNoError(err)
		Expect(lines(listing)).To(ContainElements("config -> ..data/config", "token -> ..data/token", "namespace -> ..data/namespace"))
		Expect(execIn(ctx, f, "projected", "/mini", "cat", "/etc/all/config/mode")).To(Equal("fast"))
		Expect(execIn(ctx, f, "projected", "/mini", "cat", "/etc/all/token")).To(Equal("s3cr3t"))
		Expect(execIn(ctx, f, "projected", "/mini", "cat", "/etc/all/namespace")).To(Equal(f.Namespace))
		mounts, err := execIn(ctx, f, "projected", "/mini", "cat", "/proc/mounts")
		framework.ExpectNoError(err)
		Expect(mounts).To(MatchRegexp(`(?m)^tmpfs /etc/all tmpfs ro[, ]`))

		bad := newPod("conflict", sleeper(nil), map[string]any{"name": "all", "projected": map[string]any{"sources": []any{
			map[string]any{"configMap": map[string]any{"name": "settings", "items": []any{map[string]any{"key": "mode", "path": "dup"}}}},
			map[string]any{"secret": map[string]any{"name": "token", "items": []any{map[string]any{"key": "token", "path": "dup"}}}},
		}}})
		Expect(f.ApplyExpectError(ctx, bad)).To(ContainSubstring(`spec.volumes[0].projected: Invalid value: "token": conflicting duplicate paths`))
	})
})
