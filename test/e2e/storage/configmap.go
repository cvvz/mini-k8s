package storage

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("storage", "ConfigMap", func() {
	f := framework.NewDefaultFramework("configmap")

	It("should provide keys as environment variables, with envFrom and $(VAR) expansion", func(ctx SpecContext) {
		Expect(f.KK(ctx, "create", "configmap", "app-config", "--from-literal=greeting=hello", "--from-literal=nap=1h",
			"--from-literal=1bad=x")).To(Equal("configmap/app-config created\n"))
		f.Apply(ctx, newPod("env", sleeper(map[string]any{
			// The argument only works once $(NAP) is expanded.
			"args": []any{"sleep", "$(NAP)"},
			"env": []any{
				map[string]any{"name": "GREETING", "valueFrom": keyRef("configMapKeyRef", "app-config", "greeting")},
				map[string]any{"name": "NAP", "valueFrom": keyRef("configMapKeyRef", "app-config", "nap")},
				map[string]any{"name": "MESSAGE", "value": "$(GREETING), world; $$(GREETING); $(UNDEFINED)"},
				map[string]any{"name": "OPTIONAL", "valueFrom": map[string]any{"configMapKeyRef": map[string]any{
					"name": "missing", "key": "x", "optional": true}}},
			},
			"envFrom": []any{map[string]any{"prefix": "CFG_", "configMapRef": map[string]any{"name": "app-config"}}},
		})))
		f.WaitForRunningPods(ctx, app("env"), 1)
		out, err := execIn(ctx, f, "env", "/mini", "env")
		framework.ExpectNoError(err)
		Expect(lines(out)).To(ContainElements("GREETING=hello", "MESSAGE=hello, world; $(GREETING); $(UNDEFINED)",
			"CFG_greeting=hello", "CFG_nap=1h", "CFG_1bad=x"))
		Expect(out).NotTo(ContainSubstring("OPTIONAL="))
	})

	It("should project keys into read-only volume files that follow updates", func(ctx SpecContext) {
		f.Apply(ctx, configMap("settings", map[string]any{"a": "1", "b": "2"}))
		dir := GinkgoT().TempDir()
		framework.ExpectNoError(os.WriteFile(filepath.Join(dir, "app.properties"), []byte("color=blue\n"), 0o600))
		f.KK(ctx, "create", "configmap", "files", "--from-file="+dir)
		f.Apply(ctx, newPod("reader", sleeper(map[string]any{"volumeMounts": []any{mount("cfg", "/etc/config"), mount("items", "/etc/items")}}),
			map[string]any{"name": "cfg", "configMap": map[string]any{"name": "settings"}},
			map[string]any{"name": "items", "configMap": map[string]any{"name": "files",
				"items": []any{map[string]any{"key": "app.properties", "path": "sub/dir/app.properties", "mode": 0o600}}}}))
		f.WaitForRunningPods(ctx, app("reader"), 1)
		Expect(execIn(ctx, f, "reader", "/mini", "cat", "/etc/config/a")).To(Equal("1"))
		Expect(execIn(ctx, f, "reader", "/mini", "cat", "/etc/items/sub/dir/app.properties")).To(Equal("color=blue\n"))

		By("checking the atomic-writer layout: a timestamped directory behind ..data, and links per key")
		listing, err := execIn(ctx, f, "reader", "/mini", "ls", "/etc/config")
		framework.ExpectNoError(err)
		Expect(lines(listing)).To(ContainElements(MatchRegexp(`^\.\.\d{4}_\d\d_\d\d_\d\d_\d\d_\d\d\.\d+/$`),
			MatchRegexp(`^\.\.data -> \.\.\d{4}_`), "a -> ..data/a", "b -> ..data/b"))
		Expect(execIn(ctx, f, "reader", "/mini", "ls", "/etc/items")).To(ContainSubstring("sub -> ..data/sub\n"))
		out, err := execIn(ctx, f, "reader", "/mini", "write", "/etc/config/new", "x")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("read-only file system"))

		By("changing, adding and removing keys")
		f.Apply(ctx, configMap("settings", map[string]any{"a": "changed", "b": nil, "c": "3"}))
		Eventually(ctx, func() (string, error) { return execIn(ctx, f, "reader", "/mini", "cat", "/etc/config/a") }).
			WithTimeout(30 * time.Second).Should(Equal("changed"))
		listing, err = execIn(ctx, f, "reader", "/mini", "ls", "/etc/config")
		framework.ExpectNoError(err)
		Expect(lines(listing)).To(ContainElement("c -> ..data/c"))
		Expect(lines(listing)).NotTo(ContainElement("b -> ..data/b"))
	})

	It("should keep Pods waiting for a missing ConfigMap or key and start them once it exists", func(ctx SpecContext) {
		f.Apply(ctx,
			newPod("needs-env", sleeper(map[string]any{"env": []any{
				map[string]any{"name": "K", "valueFrom": keyRef("configMapKeyRef", "late", "k")}}})),
			newPod("needs-volume", sleeper(map[string]any{"volumeMounts": []any{mount("cfg", "/etc/config")}}),
				map[string]any{"name": "cfg", "configMap": map[string]any{"name": "late"}}),
			newPod("optional", sleeper(map[string]any{"volumeMounts": []any{mount("cfg", "/etc/config")},
				"envFrom": []any{map[string]any{"configMapRef": map[string]any{"name": "late", "optional": true}}}}),
				map[string]any{"name": "cfg", "configMap": map[string]any{"name": "late", "optional": true}}))

		By("reporting a missing ConfigMap for env as CreateContainerConfigError")
		waiting := func(name, reason string) *corev1.Pod {
			return f.WaitForPod(ctx, name, "waits with "+reason, func(p *corev1.Pod) bool {
				cs := p.Status.ContainerStatuses
				return len(cs) == 1 && cs[0].State.Waiting != nil && cs[0].State.Waiting.Reason == reason
			})
		}
		Expect(waiting("needs-env", "CreateContainerConfigError").Status.ContainerStatuses[0].State.Waiting.Message).
			To(Equal(`configmap "late" not found`))
		Expect(f.KK(ctx, "get", "pod", "needs-env")).To(MatchRegexp(`needs-env\s+0/1\s+CreateContainerConfigError`))

		By("reporting a missing ConfigMap volume as FailedMount while the Pod stays ContainerCreating")
		f.WaitForEvent(ctx, "needs-volume", "FailedMount")
		Expect(f.Events(ctx, "needs-volume")).To(ContainElement(HaveField("Message",
			`MountVolume.SetUp failed for volume "cfg" : configmap "late" not found`)))
		waiting("needs-volume", "ContainerCreating")

		By("running the Pod that marks the ConfigMap optional, with an empty volume")
		f.WaitForRunningPods(ctx, app("optional"), 1)
		Expect(execIn(ctx, f, "optional", "/mini", "ls", "/etc/config")).NotTo(ContainSubstring("-> ..data/"))

		By("creating the ConfigMap, which lets the waiting Pods start")
		f.KK(ctx, "create", "configmap", "late", "--from-literal=k=v")
		f.WaitForRunningPods(ctx, app("needs-env"), 1)
		f.WaitForRunningPods(ctx, app("needs-volume"), 1)
		out, err := execIn(ctx, f, "needs-env", "/mini", "env")
		framework.ExpectNoError(err)
		Expect(lines(out)).To(ContainElement("K=v"))
		Expect(execIn(ctx, f, "needs-volume", "/mini", "cat", "/etc/config/k")).To(Equal("v"))
		Eventually(ctx, func() (string, error) { return execIn(ctx, f, "optional", "/mini", "cat", "/etc/config/k") }).
			WithTimeout(30 * time.Second).Should(Equal("v"))
	})

	It("should refuse to change the data of an immutable ConfigMap", func(ctx SpecContext) {
		frozen := configMap("frozen", map[string]any{"k": "v"})
		frozen["immutable"] = true
		f.Apply(ctx, frozen)
		frozen["data"] = map[string]any{"k": "w"}
		Expect(f.ApplyExpectError(ctx, frozen)).To(ContainSubstring("data: Forbidden: field is immutable when `immutable` is set"))
		frozen["data"], frozen["immutable"] = map[string]any{"k": "v"}, false
		Expect(f.ApplyExpectError(ctx, frozen)).To(ContainSubstring("immutable: Forbidden: field is immutable when `immutable` is set"))
		Expect(f.KK(ctx, "get", "configmap", "frozen")).To(MatchRegexp(`(?m)^frozen\s+1\s+`))
		bad := configMap("bad", map[string]any{"a/b": "x"})
		Expect(f.ApplyExpectError(ctx, bad)).To(ContainSubstring(`data[a/b]: Invalid value: "a/b": a valid config key must consist of alphanumeric characters`))
		Expect(strings.Count(f.KK(ctx, "get", "configmaps"), "\n")).To(Equal(2), "only frozen is stored")
	})
})
