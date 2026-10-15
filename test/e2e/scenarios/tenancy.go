package scenarios

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mini-k8s/test/e2e/framework"
)

const tokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

var _ = framework.SIGDescribe("scenarios", "Multi-tenancy", func() {
	f := framework.NewDefaultFramework("tenancy")

	// fromPod GETs an API path inside a Pod with its service account token
	// and returns the status code.
	fromPod := func(ctx context.Context, pod, path string) string {
		out, err := framework.RunKK(ctx, "exec", "-n", f.Namespace, pod, "--", "/mini", "fetch", "--token-file", tokenFile,
			"http://kubernetes.default.svc:443"+path)
		framework.ExpectNoError(err)
		code, _, _ := strings.Cut(out, "\n")
		return code
	}

	It("should confine a team's service account to its namespace while it deploys there, and widen it only through a ClusterRoleBinding", func(ctx SpecContext) {
		other := f.CreateNamespace(ctx, "other")
		user := "system:serviceaccount:" + f.Namespace + ":deployer"
		as := func(args ...string) (string, error) { return framework.RunKK(ctx, append(args, "--as", user)...) }

		By("binding the deployer service account to the edit ClusterRole in its own namespace")
		f.KK(ctx, "create", "serviceaccount", "deployer")
		f.KK(ctx, "create", "rolebinding", "deployer", "--clusterrole=edit", "--serviceaccount="+f.Namespace+":deployer")

		By("deploying and scaling an application as the service account")
		labels := map[string]string{"app": "api"}
		manifest := framework.WriteManifest(
			framework.Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "api"}, "data": map[string]any{"MODE": "team"}},
			framework.NewDeployment("api", 1, labels, framework.DemoContainer()), framework.NewService("api", labels, 8080))
		Expect(as("apply", "-n", f.Namespace, "-f", manifest)).To(ContainSubstring("deployment.apps/api created"))
		Expect(as("scale", "deployment", "api", "--replicas", "2", "-n", f.Namespace)).To(Equal("deployment.apps/api scaled\n"))
		f.WaitForRunningPods(ctx, labels, 2)

		By("refusing it the same in another namespace and on cluster-scoped resources")
		out, err := as("apply", "-n", other, "-f", manifest)
		Expect(err).To(HaveOccurred())
		Expect(out).To(And(ContainSubstring("forbidden"), ContainSubstring(`User "`+user+`"`), ContainSubstring(`in the namespace "`+other+`"`)))
		out, err = as("get", "pods", "-n", other)
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring(`pods is forbidden: User "` + user + `" cannot list resource "pods" in API group "" in the namespace "` + other + `"`))
		out, err = as("get", "nodes")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring(`nodes is forbidden: User "` + user + `" cannot list resource "nodes" in API group "" at the cluster scope`))

		By("giving a Pod the service account's token, which has the same rights")
		pod := framework.NewPod("client", map[string]string{"app": "client"}, framework.MiniContainer("app", "sleep", "1h"))
		pod.Spec()["serviceAccountName"] = "deployer"
		f.Apply(ctx, pod)
		f.WaitForRunningPods(ctx, map[string]string{"app": "client"}, 1)
		Expect(fromPod(ctx, "client", "/apis/apps/v1/namespaces/"+f.Namespace+"/deployments/api")).To(Equal("200"))
		Expect(fromPod(ctx, "client", "/api/v1/namespaces/"+other+"/pods")).To(Equal("403"))
		Expect(fromPod(ctx, "client", "/api/v1/nodes")).To(Equal("403"))

		By("letting it read nodes through a ClusterRole and a ClusterRoleBinding")
		role := "e2e-" + f.Namespace + "-node-reader"
		DeferCleanup(func(ctx SpecContext) {
			framework.KK(ctx, "delete", "clusterrolebinding", role, "--ignore-not-found")
			framework.KK(ctx, "delete", "clusterrole", role, "--ignore-not-found")
		})
		framework.KK(ctx, "create", "clusterrole", role, "--verb=get,list,watch", "--resource=nodes")
		framework.KK(ctx, "create", "clusterrolebinding", role, "--clusterrole="+role, "--serviceaccount="+f.Namespace+":deployer")
		Eventually(ctx, func() string { return fromPod(ctx, "client", "/api/v1/nodes") }).WithTimeout(10 * time.Second).Should(Equal("200"))
		Expect(as("get", "nodes")).To(ContainSubstring("worker-1"))
		Expect(fromPod(ctx, "client", "/api/v1/namespaces/"+other+"/pods")).To(Equal("403"), "nodes only")

		By("taking the right away with the ClusterRoleBinding")
		framework.KK(ctx, "delete", "clusterrolebinding", role)
		Eventually(ctx, func() string { return fromPod(ctx, "client", "/api/v1/nodes") }).WithTimeout(10 * time.Second).Should(Equal("403"))

		By("ending every right of its tokens when the service account is deleted")
		f.KK(ctx, "delete", "serviceaccount", "deployer")
		Eventually(ctx, func() string {
			return fromPod(ctx, "client", "/apis/apps/v1/namespaces/"+f.Namespace+"/deployments/api")
		}).
			WithTimeout(10 * time.Second).Should(Equal("401"))
		Expect(f.WaitForRunningPods(ctx, labels, 2)).To(HaveLen(2), "what it deployed keeps running")
	})
})
