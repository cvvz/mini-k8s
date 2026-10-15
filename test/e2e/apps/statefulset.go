package apps

import (
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

// newStatefulSet builds a StatefulSet of name governed by the headless
// Service of the same name; spec holds extra fields.
func newStatefulSet(name string, replicas int, c framework.Container, spec map[string]any) framework.Object {
	labels := map[string]string{"app": name}
	spec["replicas"] = replicas
	spec["serviceName"] = name
	spec["selector"] = map[string]any{"matchLabels": labels}
	spec["template"] = map[string]any{"metadata": map[string]any{"labels": labels},
		"spec": map[string]any{"terminationGracePeriodSeconds": 1, "containers": []any{framework.ContainerObject(c)}}}
	return framework.Object{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]any{"name": name}, "spec": spec}
}

func headlessService(name string) framework.Object {
	svc := framework.NewService(name, map[string]string{"app": name}, 8080)
	svc.Spec()["clusterIP"] = "None"
	return svc
}

// dataClaims is a volumeClaimTemplates list with one claim of the default
// StorageClass.
func dataClaims() []any {
	return []any{map[string]any{"metadata": map[string]any{"name": "data"}, "spec": map[string]any{
		"accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "64Mi"}}}}}
}

var _ = framework.SIGDescribe("apps", "StatefulSet", func() {
	f := framework.NewDefaultFramework("statefulset")

	readyWeb := func() framework.Container {
		c := framework.DemoContainer()
		c.Fields = map[string]any{"readinessProbe": map[string]any{"periodSeconds": 1,
			"httpGet": map[string]any{"path": "/ready", "port": 8080}}}
		return c
	}
	sleeper := func(env string) framework.Container {
		c := framework.MiniContainer("app", "sleep", "1h")
		c.Env = map[string]string{"VERSION": env}
		return c
	}
	// live maps the names of the set's live Pods to their revision.
	live := func(ctx SpecContext, name string) map[string]string {
		list, err := client.Pods(f.Client, f.Namespace).List(ctx, client.ListOptions{LabelSelector: "app=" + name})
		framework.ExpectNoError(err)
		out := map[string]string{}
		for _, p := range list.Items {
			if p.DeletionTimestamp == nil {
				out[p.Name] = p.Labels[appsv1.ControllerRevisionHashLabelKey]
			}
		}
		return out
	}
	inPod := func(ctx SpecContext, pod string, args ...string) (string, error) {
		return framework.RunKK(ctx, append([]string{"exec", "-n", f.Namespace, pod, "--", "/mini"}, args...)...)
	}
	// eventsInOrder lists the set's event messages with a reason, oldest first.
	eventsInOrder := func(ctx SpecContext, name, reason string) []string {
		events := f.Events(ctx, name)
		sort.SliceStable(events, func(i, j int) bool { return events[i].FirstTimestamp.Before(events[j].FirstTimestamp.Time) })
		var out []string
		for _, e := range events {
			if e.Reason == reason {
				out = append(out, e.Message)
			}
		}
		return out
	}
	claims := func(ctx SpecContext) []string {
		list, err := client.PersistentVolumeClaims(f.Client, f.Namespace).List(ctx, client.ListOptions{})
		framework.ExpectNoError(err)
		var out []string
		for _, c := range list.Items {
			if c.DeletionTimestamp == nil {
				out = append(out, c.Name+"="+c.Status.Phase)
			}
		}
		sort.Strings(out)
		return out
	}

	It("should create Pods one by one with stable names and DNS identities", func(ctx SpecContext) {
		f.Apply(ctx, headlessService("web"), newStatefulSet("web", 3, readyWeb(), map[string]any{}))
		Expect(f.KK(ctx, "rollout", "status", "statefulset/web", "--timeout", "120s")).
			To(HaveSuffix("partitioned roll out complete: 3 new pods have been updated...\n"))
		Expect(eventsInOrder(ctx, "web", "SuccessfulCreate")).To(Equal([]string{"Create Pod web-0 in StatefulSet web successful",
			"Create Pod web-1 in StatefulSet web successful", "Create Pod web-2 in StatefulSet web successful"}))
		Expect(f.KK(ctx, "get", "statefulset", "web")).To(MatchRegexp(`(?m)^web\s+3/3\s+`))
		pods := map[string]corev1.Pod{}
		list, err := client.Pods(f.Client, f.Namespace).List(ctx, client.ListOptions{LabelSelector: "app=web"})
		framework.ExpectNoError(err)
		for _, p := range list.Items {
			pods[p.Name] = p
		}
		Expect(pods).To(HaveLen(3))
		for i, name := range []string{"web-0", "web-1", "web-2"} {
			p := pods[name]
			Expect(p.Spec.Hostname).To(Equal(name))
			Expect(p.Spec.Subdomain).To(Equal("web"))
			Expect(p.Labels).To(HaveKeyWithValue(appsv1.StatefulSetPodNameLabel, name))
			Expect(p.Labels).To(HaveKeyWithValue(appsv1.PodIndexLabel, string(rune('0'+i))))
		}
		hosts, err := inPod(ctx, "web-0", "cat", "/etc/hosts")
		framework.ExpectNoError(err)
		Expect(hosts).To(ContainSubstring("web-0.web." + f.Namespace + ".svc.cluster.local\tweb-0"))

		By("resolving each Pod by name under the headless Service")
		Eventually(ctx, func() (string, error) { return inPod(ctx, "web-0", "lookup", "web-1.web") }).
			WithTimeout(30 * time.Second).Should(Equal(pods["web-1"].Status.PodIP + "\n"))
		Expect(inPod(ctx, "web-1", "fetch", "http://web-2.web:8080/")).To(ContainSubstring("web-2"))

		By("deleting web-1: it comes back with the same name and DNS name")
		f.KK(ctx, "delete", "pod", "web-1")
		Eventually(ctx, func() string {
			p, err := client.Pods(f.Client, f.Namespace).Get(ctx, "web-1")
			if err != nil || p.UID == pods["web-1"].UID || !corev1.IsPodReady(p) {
				return ""
			}
			return p.Status.PodIP
		}).WithTimeout(time.Minute).ShouldNot(BeEmpty())
		p, err := client.Pods(f.Client, f.Namespace).Get(ctx, "web-1")
		framework.ExpectNoError(err)
		Eventually(ctx, func() (string, error) { return inPod(ctx, "web-0", "lookup", "web-1.web") }).
			WithTimeout(30 * time.Second).Should(Equal(p.Status.PodIP + "\n"))
	})

	It("should keep each Pod's claim across restarts and scaling", func(ctx SpecContext) {
		set := newStatefulSet("notes", 2, sleeper("1"), map[string]any{"volumeClaimTemplates": dataClaims()})
		set.PodSpec()["containers"].([]any)[0].(map[string]any)["volumeMounts"] = []any{map[string]any{"name": "data", "mountPath": "/data"}}
		f.Apply(ctx, headlessService("notes"), set)
		f.KK(ctx, "rollout", "status", "statefulset/notes", "--timeout", "180s")
		Expect(claims(ctx)).To(Equal([]string{"data-notes-0=Bound", "data-notes-1=Bound"}))
		Expect(eventsInOrder(ctx, "notes", "SuccessfulCreate")).To(ContainElement("Create Claim data-notes-0 Pod notes-0 in StatefulSet notes success"))
		for _, name := range []string{"notes-0", "notes-1"} {
			_, err := inPod(ctx, name, "write", "/data/owner", name)
			framework.ExpectNoError(err)
		}

		By("replacing notes-0: the new Pod gets the same claim")
		old, err := client.Pods(f.Client, f.Namespace).Get(ctx, "notes-0")
		framework.ExpectNoError(err)
		f.KK(ctx, "delete", "pod", "notes-0")
		Eventually(ctx, func() bool {
			p, err := client.Pods(f.Client, f.Namespace).Get(ctx, "notes-0")
			return err == nil && p.UID != old.UID && corev1.IsPodReady(p)
		}).WithTimeout(2 * time.Minute).Should(BeTrue())
		Expect(inPod(ctx, "notes-0", "cat", "/data/owner")).To(Equal("notes-0\n"))

		By("scaling to 1 and back: the claim of notes-1 is kept and reused")
		Expect(f.KK(ctx, "scale", "statefulset", "notes", "--replicas", "1")).To(Equal("statefulset.apps/notes scaled\n"))
		Eventually(ctx, func() map[string]string { return live(ctx, "notes") }).WithTimeout(time.Minute).Should(HaveLen(1))
		Expect(claims(ctx)).To(HaveLen(2))
		f.KK(ctx, "scale", "statefulset", "notes", "--replicas", "2")
		f.KK(ctx, "rollout", "status", "statefulset/notes", "--timeout", "120s")
		Expect(inPod(ctx, "notes-1", "cat", "/data/owner")).To(Equal("notes-1\n"))
	})

	It("should update Pods from the highest ordinal down to the partition and roll back", func(ctx SpecContext) {
		f.Apply(ctx, newStatefulSet("roll", 3, sleeper("1"), map[string]any{}))
		f.KK(ctx, "rollout", "status", "statefulset/roll", "--timeout", "120s")
		first := live(ctx, "roll")["roll-0"]

		By("updating with partition 2: only roll-2 changes")
		f.Apply(ctx, newStatefulSet("roll", 3, sleeper("2"), map[string]any{
			"updateStrategy": map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"partition": 2}}}))
		Expect(f.KK(ctx, "rollout", "status", "statefulset/roll", "--timeout", "120s")).
			To(HaveSuffix("partitioned roll out complete: 1 new pods have been updated...\n"))
		pods := live(ctx, "roll")
		second := pods["roll-2"]
		Expect(second).NotTo(Equal(first))
		Expect(pods["roll-0"]).To(Equal(first))
		Expect(pods["roll-1"]).To(Equal(first))
		s, err := client.StatefulSets(f.Client, f.Namespace).Get(ctx, "roll")
		framework.ExpectNoError(err)
		Expect(s.Status.CurrentRevision).To(Equal(first))
		Expect(s.Status.UpdateRevision).To(Equal(second))

		By("lowering the partition: the rest follow, highest ordinal first")
		f.Apply(ctx, newStatefulSet("roll", 3, sleeper("2"), map[string]any{
			"updateStrategy": map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"partition": 0}}}))
		f.KK(ctx, "rollout", "status", "statefulset/roll", "--timeout", "120s")
		Eventually(ctx, func() map[string]string { return live(ctx, "roll") }).WithTimeout(time.Minute).
			Should(Equal(map[string]string{"roll-0": second, "roll-1": second, "roll-2": second}))
		Expect(eventsInOrder(ctx, "roll", "SuccessfulDelete")).To(Equal([]string{"Delete Pod roll-2 in StatefulSet roll successful",
			"Delete Pod roll-1 in StatefulSet roll successful", "Delete Pod roll-0 in StatefulSet roll successful"}))
		Expect(f.KK(ctx, "rollout", "history", "statefulset/roll")).To(MatchRegexp(`REVISION\s+CHANGE-CAUSE\n1\s+<none>\n2\s+<none>`))

		By("rolling back to the first revision")
		Expect(f.KK(ctx, "rollout", "undo", "statefulset/roll")).To(Equal("statefulset.apps/roll rolled back\n"))
		Eventually(ctx, func() map[string]string { return live(ctx, "roll") }).WithTimeout(2 * time.Minute).
			Should(Equal(map[string]string{"roll-0": first, "roll-1": first, "roll-2": first}))
		Expect(f.KK(ctx, "get", "controllerrevisions")).To(MatchRegexp(`(?m)^` + first + `\s+statefulset.apps/roll\s+3\s+`))
	})

	It("should start Pods in parallel and delete claims as the retention policy says", func(ctx SpecContext) {
		slow := sleeper("1")
		slow.Fields = map[string]any{"readinessProbe": map[string]any{"initialDelaySeconds": 20, "periodSeconds": 1,
			"exec": map[string]any{"command": []any{"/mini", "ls", "/"}}}}
		set := newStatefulSet("par", 3, slow, map[string]any{"podManagementPolicy": "Parallel", "volumeClaimTemplates": dataClaims(),
			"persistentVolumeClaimRetentionPolicy": map[string]any{"whenDeleted": "Delete", "whenScaled": "Delete"}})
		set.PodSpec()["containers"].([]any)[0].(map[string]any)["volumeMounts"] = []any{map[string]any{"name": "data", "mountPath": "/data"}}
		f.Apply(ctx, set)
		By("all three Pods start before any is ready")
		Eventually(ctx, func() int {
			list, err := client.Pods(f.Client, f.Namespace).List(ctx, client.ListOptions{LabelSelector: "app=par"})
			framework.ExpectNoError(err)
			for _, p := range list.Items {
				Expect(corev1.IsPodReady(&p)).To(BeFalse(), "%s became ready before all Pods existed", p.Name)
			}
			return len(list.Items)
		}).WithTimeout(15 * time.Second).Should(Equal(3))
		f.KK(ctx, "rollout", "status", "statefulset/par", "--timeout", "180s")
		Expect(claims(ctx)).To(Equal([]string{"data-par-0=Bound", "data-par-1=Bound", "data-par-2=Bound"}))

		By("scaling to 1: the claims of the removed Pods are deleted")
		f.KK(ctx, "scale", "statefulset", "par", "--replicas", "1")
		Eventually(ctx, func() []string { return claims(ctx) }).WithTimeout(2 * time.Minute).Should(Equal([]string{"data-par-0=Bound"}))

		By("deleting the set: its last claim goes too")
		f.KK(ctx, "delete", "statefulset", "par")
		Eventually(ctx, func() []string { return claims(ctx) }).WithTimeout(2 * time.Minute).Should(BeEmpty())
	})

	It("should reject invalid StatefulSets and changes to fixed fields", func(ctx SpecContext) {
		bad := newStatefulSet("bad", 1, sleeper("1"), map[string]any{"podManagementPolicy": "Sometimes"})
		bad.Spec()["replicas"] = -1
		out := f.ApplyExpectError(ctx, bad)
		Expect(out).To(ContainSubstring("spec.replicas: Invalid value: -1: must be greater than or equal to 0"))
		Expect(out).To(ContainSubstring(`spec.podManagementPolicy: Invalid value: "Sometimes": must be 'OrderedReady' or 'Parallel'`))

		f.Apply(ctx, newStatefulSet("fixed", 0, sleeper("1"), map[string]any{}))
		changed := newStatefulSet("fixed", 0, sleeper("1"), map[string]any{})
		changed.Spec()["serviceName"] = "other"
		Expect(f.ApplyExpectError(ctx, changed)).To(ContainSubstring("spec: Forbidden: updates to statefulset spec for fields other than " +
			"'replicas', 'ordinals', 'template', 'updateStrategy', 'revisionHistoryLimit', 'persistentVolumeClaimRetentionPolicy' and " +
			"'minReadySeconds' are forbidden"))
		Expect(strings.TrimSpace(f.KK(ctx, "get", "sts", "fixed"))).To(MatchRegexp(`fixed\s+0/0\s+`))
	})
})
