// Package apimachinery holds API server contract specs.
package apimachinery

import (
	"encoding/json"
	"errors"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mini-k8s/internal/mini"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("api-machinery", "API server", func() {
	f := framework.NewDefaultFramework("api")

	It("should reject writes based on a stale resourceVersion", func(ctx SpecContext) {
		name := f.UniqueName("rv")
		labels := map[string]string{"app": name}
		f.Apply(ctx, framework.NewDeployment(name, 0, labels, framework.DemoContainer()))

		stale, err := f.Client.Get(ctx, "Deployment", name)
		framework.ExpectNoError(err)
		fresh := stale
		var spec mini.DeploymentSpec
		framework.ExpectNoError(json.Unmarshal(fresh.Spec, &spec))
		replicas := 1
		spec.Replicas = &replicas
		fresh.Spec, err = json.Marshal(spec)
		framework.ExpectNoError(err)
		framework.ExpectNoError(f.Client.Update(ctx, fresh, false))

		err = f.Client.Update(ctx, stale, false)
		Expect(errors.Is(err, mini.ErrConflict)).To(BeTrue(), "expected a conflict, got %v", err)
		err = f.Client.Delete(ctx, stale)
		Expect(errors.Is(err, mini.ErrConflict)).To(BeTrue(), "stale delete should conflict, got %v", err)
	})

	DescribeTable("should reject invalid manifests before calling the API",
		func(ctx SpecContext, mutate func(framework.Object), want string) {
			name := f.UniqueName("invalid")
			obj := framework.NewDeployment(name, 1, map[string]string{"app": name}, framework.DemoContainer())
			mutate(obj)
			out := f.ApplyExpectError(ctx, obj)
			Expect(out).To(ContainSubstring(want))
			_, err := f.Client.Get(ctx, "Deployment", name)
			Expect(errors.Is(err, mini.ErrNotFound)).To(BeTrue(), "invalid object must not be stored")
		},
		Entry("unknown field", func(o framework.Object) { o["spec"].(map[string]any)["paused"] = true }, "unknown field"),
		Entry("replicas above 20", func(o framework.Object) { o["spec"].(map[string]any)["replicas"] = 21 }, "between 0 and 20"),
		Entry("server-managed uid", func(o framework.Object) { o["metadata"].(map[string]any)["uid"] = "x" }, "server-managed"),
		Entry("unsupported namespace", func(o framework.Object) { o["metadata"].(map[string]any)["namespace"] = "prod" }, "only namespace default"),
	)

	It("should reject requests without the bearer token", func(ctx SpecContext) {
		code, _, err := framework.HTTPGet(ctx, framework.TestContext.APIURL+"/api/Pod")
		framework.ExpectNoError(err)
		Expect(code).To(Equal(http.StatusUnauthorized))
	})
})
