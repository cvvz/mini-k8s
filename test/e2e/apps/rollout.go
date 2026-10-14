package apps

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

// versioned is the demo container with VERSION=v and a readiness probe.
func versioned(v string) framework.Container {
	c := framework.DemoContainer()
	c.Env = map[string]string{"VERSION": v}
	c.Fields = map[string]any{"readinessProbe": map[string]any{
		"httpGet": map[string]any{"path": "/ready", "port": 8080}, "periodSeconds": 1}}
	return c
}

// versionsOf counts the VERSION of every Pod, terminating ones included.
func versionsOf(ctx SpecContext, f *framework.Framework, app string) map[string]int {
	return countVersions(ctx, f, app, true)
}

// liveVersions counts the VERSION of the Pods that are not terminating (a
// finished rollout may still be stopping old Pods, as in Kubernetes).
func liveVersions(ctx SpecContext, f *framework.Framework, app string) map[string]int {
	return countVersions(ctx, f, app, false)
}

func countVersions(ctx SpecContext, f *framework.Framework, app string, terminating bool) map[string]int {
	list, err := client.Pods(f.Client, f.Namespace).List(ctx, client.ListOptions{LabelSelector: "app=" + app})
	framework.ExpectNoError(err)
	out := map[string]int{}
	for _, p := range list.Items {
		if p.DeletionTimestamp != nil && !terminating {
			continue
		}
		for _, e := range p.Spec.Containers[0].Env {
			if e.Name == "VERSION" {
				out[e.Value]++
			}
		}
	}
	return out
}

var _ = framework.SIGDescribe("apps", "Deployment rollouts", func() {
	f := framework.NewDefaultFramework("rollout")

	rollout := func(ctx SpecContext, args ...string) string {
		return framework.KK(ctx, append([]string{"rollout", "-n", f.Namespace}, args...)...)
	}
	deploy := func(name, version string, mutate func(framework.Object)) framework.Object {
		obj := framework.NewDeployment(name, 3, map[string]string{"app": name}, versioned(version))
		if mutate != nil {
			mutate(obj)
		}
		return obj
	}
	surge1 := func(o framework.Object) {
		o.Spec()["strategy"] = map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": 1, "maxUnavailable": 0}}
	}

	It("should roll out a new template one Pod at a time and keep every revision", func(ctx SpecContext) {
		f.Apply(ctx, deploy("web", "1", surge1))
		Expect(rollout(ctx, "status", "deployment/web", "--timeout", "90s")).To(ContainSubstring(`deployment "web" successfully rolled out`))

		f.Apply(ctx, deploy("web", "2", surge1))
		Expect(rollout(ctx, "status", "deployment/web", "--timeout", "90s")).To(And(
			ContainSubstring(`Waiting for deployment "web" rollout to finish:`),
			HaveSuffix("deployment \"web\" successfully rolled out\n")))
		Expect(liveVersions(ctx, f, "web")).To(Equal(map[string]int{"2": 3}))

		Expect(framework.KK(ctx, "get", "rs", "-n", f.Namespace)).To(And(
			MatchRegexp(`web-\w+\s+3\s+3\s+3`), MatchRegexp(`web-\w+\s+0\s+0\s+0`)))
		Expect(rollout(ctx, "history", "deployment/web")).To(MatchRegexp(`(?s)deployment.apps/web \nREVISION\s+CHANGE-CAUSE\n1\s+<none>\n2\s+<none>\n`))
		var scaling []string
		for _, e := range f.Events(ctx, "web") {
			if e.Reason == "ScalingReplicaSet" {
				scaling = append(scaling, e.Message)
			}
		}
		Expect(scaling).To(ContainElement(MatchRegexp(`^Scaled up replica set web-\w+ from 0 to 1$`)), "maxSurge 1: one new Pod at a time")
		Expect(scaling).To(ContainElement(MatchRegexp(`^Scaled down replica set web-\w+ from 3 to 2$`)))

		d, err := client.Deployments(f.Client, f.Namespace).Get(ctx, "web")
		framework.ExpectNoError(err)
		Expect(d.Annotations).To(HaveKeyWithValue(appsv1.RevisionAnnotation, "2"))
		Expect(d.Status.Condition(appsv1.DeploymentProgressing).Reason).To(Equal("NewReplicaSetAvailable"))
		Expect(d.Status.Condition(appsv1.DeploymentAvailable).Status).To(Equal(corev1.ConditionTrue))
	})

	It("should roll back to the previous or a given revision", func(ctx SpecContext) {
		cause := func(c string) func(framework.Object) {
			return func(o framework.Object) { o.Meta()["annotations"] = map[string]any{appsv1.ChangeCauseAnnotation: c} }
		}
		f.Apply(ctx, deploy("back", "1", cause("first")))
		rollout(ctx, "status", "deployment/back", "--timeout", "90s")
		f.Apply(ctx, deploy("back", "2", cause("second")))
		rollout(ctx, "status", "deployment/back", "--timeout", "90s")

		Expect(rollout(ctx, "undo", "deployment/back")).To(Equal("deployment.apps/back rolled back\n"))
		rollout(ctx, "status", "deployment/back", "--timeout", "90s")
		Expect(liveVersions(ctx, f, "back")).To(Equal(map[string]int{"1": 3}))
		Expect(rollout(ctx, "history", "deployment/back")).To(MatchRegexp(`(?s)2\s+second\n3\s+first\n`), "the restored revision becomes revision 3")

		_, err := framework.RunKK(ctx, "rollout", "-n", f.Namespace, "undo", "deployment/back", "--to-revision", "9")
		Expect(err).To(MatchError(ContainSubstring("unable to find specified revision 9 in history")))
		Expect(rollout(ctx, "undo", "deployment/back", "--to-revision", "2")).To(Equal("deployment.apps/back rolled back\n"))
		rollout(ctx, "status", "deployment/back", "--timeout", "90s")
		Expect(liveVersions(ctx, f, "back")).To(Equal(map[string]int{"2": 3}))
	})

	It("should hold template changes while paused and roll them out when resumed", func(ctx SpecContext) {
		f.Apply(ctx, deploy("hold", "1", nil))
		rollout(ctx, "status", "deployment/hold", "--timeout", "90s")
		Expect(rollout(ctx, "pause", "deployment/hold")).To(Equal("deployment.apps/hold paused\n"))
		f.Apply(ctx, deploy("hold", "2", nil))
		Consistently(ctx, func() map[string]int { return versionsOf(ctx, f, "hold") }).
			WithTimeout(4 * time.Second).Should(Equal(map[string]int{"1": 3}))
		d, err := client.Deployments(f.Client, f.Namespace).Get(ctx, "hold")
		framework.ExpectNoError(err)
		Expect(d.Status.Condition(appsv1.DeploymentProgressing).Reason).To(Equal("DeploymentPaused"))

		Expect(rollout(ctx, "resume", "deployment/hold")).To(Equal("deployment.apps/hold resumed\n"))
		rollout(ctx, "status", "deployment/hold", "--timeout", "90s")
		Expect(liveVersions(ctx, f, "hold")).To(Equal(map[string]int{"2": 3}))
	})

	It("should replace all Pods at once with the Recreate strategy", func(ctx SpecContext) {
		recreate := func(o framework.Object) { o.Spec()["strategy"] = map[string]any{"type": "Recreate"} }
		f.Apply(ctx, deploy("rec", "1", recreate))
		rollout(ctx, "status", "deployment/rec", "--timeout", "90s")
		f.Apply(ctx, deploy("rec", "2", recreate))
		deadline := time.Now().Add(90 * time.Second)
		for {
			v := versionsOf(ctx, f, "rec")
			Expect(v["1"] > 0 && v["2"] > 0).To(BeFalse(), "old and new Pods must never coexist: %v", v)
			if v["2"] == 3 && v["1"] == 0 {
				break
			}
			Expect(time.Now().Before(deadline)).To(BeTrue(), "rollout did not finish: %v", v)
			time.Sleep(200 * time.Millisecond)
		}
		rollout(ctx, "status", "deployment/rec", "--timeout", "90s")
	})

	It("should restart every Pod with kk rollout restart", func(ctx SpecContext) {
		f.Apply(ctx, deploy("fresh", "1", nil))
		rollout(ctx, "status", "deployment/fresh", "--timeout", "90s")
		before := f.WaitForRunningPods(ctx, map[string]string{"app": "fresh"}, 3)
		Expect(rollout(ctx, "restart", "deployment/fresh")).To(Equal("deployment.apps/fresh restarted\n"))
		rollout(ctx, "status", "deployment/fresh", "--timeout", "90s")
		after := f.WaitForRunningPods(ctx, map[string]string{"app": "fresh"}, 3)
		for _, p := range after {
			for _, b := range before {
				Expect(p.UID).NotTo(Equal(b.UID))
			}
			Expect(p.Annotations).To(HaveKey(appsv1.RestartedAtAnnotation))
		}
	})
})

var _ = framework.SIGDescribe("apps", "ReplicaSet", func() {
	f := framework.NewDefaultFramework("replicaset")

	It("should keep its replicas running and scale with kk", func(ctx SpecContext) {
		labels := map[string]string{"app": "plain-rs"}
		rs := framework.NewDeployment("plain-rs", 2, labels, framework.DemoContainer())
		rs["kind"] = "ReplicaSet"
		f.Apply(ctx, rs)
		pods := f.WaitForRunningPods(ctx, labels, 2)
		for _, p := range pods {
			Expect(strings.HasPrefix(p.Name, "plain-rs-")).To(BeTrue())
		}

		f.KK(ctx, "delete", "pod", pods[0].Name)
		f.WaitForPodGone(ctx, &pods[0], framework.TestContext.PodStartTimeout)
		f.WaitForRunningPods(ctx, labels, 2)
		f.WaitForEvent(ctx, "plain-rs", "SuccessfulCreate")

		Expect(f.KK(ctx, "scale", "rs", "plain-rs", "--replicas", "3")).To(Equal("replicaset.apps/plain-rs scaled\n"))
		f.WaitForRunningPods(ctx, labels, 3)
		Eventually(ctx, func() (string, error) {
			return framework.RunKK(ctx, "get", "replicasets", "-n", f.Namespace)
		}).Should(MatchRegexp(`plain-rs\s+3\s+3\s+3`))

		f.KK(ctx, "delete", "rs", "plain-rs")
		Eventually(ctx, func() ([]corev1.Pod, error) { return f.Pods(ctx, labels) }).
			WithTimeout(framework.TestContext.PodStartTimeout).Should(BeEmpty(), "its Pods are garbage-collected")
	})
})
