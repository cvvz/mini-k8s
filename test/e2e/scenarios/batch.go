package scenarios

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "mini-k8s/pkg/apis/batch/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("scenarios", "Batch pipeline", func() {
	f := framework.NewDefaultFramework("pipeline")

	// newJob is a Job of containers that mount the claim "work" at /work.
	newJob := func(name string, spec map[string]any, c framework.Container) framework.Object {
		obj := framework.ContainerObject(c)
		obj["volumeMounts"] = []any{map[string]any{"name": "work", "mountPath": "/work"}}
		spec["template"] = map[string]any{"spec": map[string]any{"restartPolicy": "Never", "containers": []any{obj},
			"volumes": []any{map[string]any{"name": "work", "persistentVolumeClaim": map[string]any{"claimName": "work"}}}}}
		return framework.Object{"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]any{"name": name}, "spec": spec}
	}
	jobPods := func(ctx SpecContext, job string) []corev1.Pod {
		list, err := client.Pods(f.Client, f.Namespace).List(ctx, client.ListOptions{LabelSelector: batchv1.JobNameLabel + "=" + job})
		framework.ExpectNoError(err)
		return list.Items
	}

	It("should fan an Indexed Job out over a shared volume and gather its results once the Job has expired", func(ctx SpecContext) {
		By("running three indexed workers at once, each writing its part to the shared claim")
		part := framework.MiniContainer("part", "write", "/work/part-$(JOB_COMPLETION_INDEX)", "$(POD_NAME)")
		part.Fields = map[string]any{"env": []any{map[string]any{"name": "POD_NAME",
			"valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.name"}}}}}
		f.Apply(ctx, newClaim("work"), newJob("fanout", map[string]any{"completionMode": "Indexed", "completions": 3,
			"parallelism": 3, "ttlSecondsAfterFinished": 3}, part))
		Eventually(ctx, func() string { return f.KK(ctx, "get", "job", "fanout") }).WithTimeout(2 * time.Minute).
			Should(MatchRegexp(`(?m)^fanout\s+Complete\s+3/3\s+`))
		workers := jobPods(ctx, "fanout")
		Expect(workers).To(HaveLen(3))
		claim, err := client.PersistentVolumeClaims(f.Client, f.Namespace).Get(ctx, "work")
		framework.ExpectNoError(err)
		Expect(claim.Status.Phase).To(Equal(corev1.ClaimBound))
		pv, err := client.PersistentVolumes(f.Client).Get(ctx, claim.Spec.VolumeName)
		framework.ExpectNoError(err)
		terms := pv.Spec.NodeAffinity.Required.NodeSelectorTerms
		Expect(terms).NotTo(BeEmpty())
		node := terms[0].MatchExpressions[0].Values[0]
		names := map[string]string{}
		for _, p := range workers {
			Expect(p.Spec.NodeName).To(Equal(node), "Pod %s runs where the local volume is", p.Name)
			names[p.Labels[batchv1.JobCompletionIndexKey]] = p.Name
		}
		Expect(names).To(HaveLen(3))

		By("deleting the finished Job and its Pods after the TTL, while the claim keeps the parts")
		Eventually(ctx, func(g Gomega) {
			_, err := client.Jobs(f.Client, f.Namespace).Get(ctx, "fanout")
			g.Expect(client.IsNotFound(err)).To(BeTrue(), "the Job still exists: %v", err)
			g.Expect(jobPods(ctx, "fanout")).To(BeEmpty())
		}).WithTimeout(time.Minute).Should(Succeed())

		By("gathering the parts with a second Job")
		f.Apply(ctx, newJob("gather", map[string]any{"backoffLimit": 0},
			framework.MiniContainer("gather", "cat", "/work/part-0", "/work/part-1", "/work/part-2")))
		Eventually(ctx, func() string { return f.KK(ctx, "get", "job", "gather") }).WithTimeout(time.Minute).
			Should(MatchRegexp(`(?m)^gather\s+Complete\s+1/1\s+`))
		gatherers := jobPods(ctx, "gather")
		Expect(gatherers).To(HaveLen(1))
		Expect(gatherers[0].Spec.NodeName).To(Equal(node))
		Expect(f.KK(ctx, "logs", gatherers[0].Name)).To(Equal(strings.Join([]string{names["0"], names["1"], names["2"]}, "\n") + "\n"))
	})
})
