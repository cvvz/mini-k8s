// Package auth holds the [sig-auth] e2e specs: service accounts, their
// tokens, and RBAC authorization.
package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	authenticationv1 "mini-k8s/pkg/apis/authentication/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/e2e/framework"
)

const tokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

var _ = framework.SIGDescribe("auth", "ServiceAccounts and RBAC", func() {
	f := framework.NewDefaultFramework("auth")

	// fromPod GETs an API path inside a Pod with its service account token
	// and returns the status code line and the body.
	fromPod := func(ctx context.Context, pod, path string) (string, string) {
		out, err := framework.RunKK(ctx, "exec", "-n", f.Namespace, pod, "--", "/mini", "fetch", "--token-file", tokenFile,
			"http://kubernetes.default.svc:443"+path)
		framework.ExpectNoError(err)
		code, body, _ := strings.Cut(out, "\n")
		return code, body
	}
	sleeper := func(name string, spec map[string]any) framework.Object {
		pod := framework.NewPod(name, map[string]string{"app": name}, framework.MiniContainer("app", "sleep", "1h"))
		for k, v := range spec {
			pod.Spec()[k] = v
		}
		return pod
	}

	It("should mount a token of the Pod's service account that authenticates as it", func(ctx SpecContext) {
		Expect(f.KK(ctx, "get", "serviceaccounts")).To(MatchRegexp(`(?m)^default\s+`), "every namespace has a default service account")
		f.Apply(ctx, sleeper("client", nil))
		pod := f.WaitForRunningPods(ctx, map[string]string{"app": "client"}, 1)[0]
		Expect(pod.Spec.ServiceAccountName).To(Equal("default"))
		Expect(pod.Spec.Containers[0].VolumeMounts).To(ContainElement(And(
			HaveField("MountPath", "/var/run/secrets/kubernetes.io/serviceaccount"), HaveField("ReadOnly", true))))
		Expect(framework.KK(ctx, "exec", "-n", f.Namespace, "client", "--", "/mini", "cat",
			"/var/run/secrets/kubernetes.io/serviceaccount/namespace")).To(Equal(f.Namespace))

		By("the default service account may discover the API but not list Pods")
		code, _ := fromPod(ctx, "client", "/apis")
		Expect(code).To(Equal("200"))
		code, body := fromPod(ctx, "client", "/api/v1/namespaces/"+f.Namespace+"/pods")
		Expect(code).To(Equal("403"))
		Expect(body).To(ContainSubstring(`pods is forbidden: User \"system:serviceaccount:` + f.Namespace +
			`:default\" cannot list resource \"pods\" in API group \"\" in the namespace \"` + f.Namespace + `\"`))

		By("a RoleBinding to the view ClusterRole lets it read Pods in its namespace only")
		f.KK(ctx, "create", "rolebinding", "viewer", "--clusterrole=view", "--serviceaccount="+f.Namespace+":default")
		Eventually(ctx, func() string {
			code, _ := fromPod(ctx, "client", "/api/v1/namespaces/"+f.Namespace+"/pods")
			return code
		}).
			WithTimeout(10 * time.Second).Should(Equal("200"))
		code, _ = fromPod(ctx, "client", "/api/v1/namespaces/kube-system/pods")
		Expect(code).To(Equal("403"))
		code, _ = fromPod(ctx, "client", "/api/v1/namespaces/"+f.Namespace+"/secrets")
		Expect(code).To(Equal("403"), "view does not read Secrets")
	})

	It("should grant exactly what Roles allow and answer kk auth can-i", func(ctx SpecContext) {
		Expect(f.KK(ctx, "create", "serviceaccount", "robot")).To(Equal("serviceaccount/robot created\n"))
		sa := "system:serviceaccount:" + f.Namespace + ":robot"
		canI := func(args ...string) string {
			out, _ := framework.RunKK(ctx, append(append([]string{"auth", "can-i"}, args...), "-n", f.Namespace, "--as", sa)...)
			return strings.TrimSpace(out)
		}
		Expect(canI("list", "pods")).To(Equal("no"))
		f.KK(ctx, "create", "role", "scaler", "--verb=get,update,patch", "--resource=deployments/scale", "--resource-name=web")
		f.KK(ctx, "create", "role", "pod-reader", "--verb=get,list,watch", "--resource=pods,pods/log")
		f.KK(ctx, "create", "rolebinding", "robot", "--role=pod-reader", "--serviceaccount="+f.Namespace+":robot")
		f.KK(ctx, "create", "rolebinding", "robot-scaler", "--role=scaler", "--serviceaccount="+f.Namespace+":robot")
		Expect(canI("list", "pods")).To(Equal("yes"))
		Expect(canI("get", "pods/log")).To(Equal("yes"))
		Expect(canI("delete", "pods")).To(Equal("no"))
		Expect(canI("update", "deployments/scale", "web")).To(Equal("yes"))
		Expect(canI("update", "deployments/scale", "db")).To(Equal("no"))
		Expect(f.KK(ctx, "describe", "rolebinding", "robot")).To(MatchRegexp(`ServiceAccount\s+robot\s+` + f.Namespace))
		Expect(f.KK(ctx, "get", "rolebindings", "-o", "wide")).To(MatchRegexp(`(?m)^robot\s+Role/pod-reader\s+\S+\s+` + f.Namespace + `/robot`))

		By("acting as the service account")
		Expect(f.KK(ctx, "auth", "whoami", "--as", sa)).To(ContainSubstring("[system:serviceaccounts system:serviceaccounts:" + f.Namespace +
			" system:authenticated]"))
		f.Apply(ctx, framework.NewDeployment("web", 1, map[string]string{"app": "web"}, framework.MiniContainer("app", "sleep", "1h")))
		Expect(f.KK(ctx, "get", "pods", "--as", sa)).To(ContainSubstring("web-"))
		out, err := framework.RunKK(ctx, "delete", "deployment", "web", "-n", f.Namespace, "--as", sa)
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring(`Error from server (Forbidden): deployments.apps "web" is forbidden: User "` + sa +
			`" cannot delete resource "deployments" in API group "apps" in the namespace "` + f.Namespace + `"`))
		Expect(f.KK(ctx, "scale", "deployment", "web", "--replicas", "2", "--as", sa)).To(Equal("deployment.apps/web scaled\n"),
			"the scale subresource of web is allowed by name")

		By("rejecting a change of roleRef")
		out, err = framework.RunKK(ctx, "apply", "-n", f.Namespace, "-f", framework.WriteManifest(framework.Object{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": map[string]any{"name": "robot"},
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "scaler"},
			"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "robot"}}}))
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("cannot change roleRef"))
	})

	It("should issue tokens that end with their service account or bound Pod", func(ctx SpecContext) {
		f.KK(ctx, "create", "serviceaccount", "batch")
		f.KK(ctx, "create", "rolebinding", "batch-view", "--clusterrole=view", "--serviceaccount="+f.Namespace+":batch")
		token := strings.TrimSpace(f.KK(ctx, "create", "token", "batch", "--duration", "15m"))
		Expect(strings.Count(token, ".")).To(Equal(2), "a JWT")
		c := client.New(framework.TestContext.APIURL, token)
		_, err := client.Pods(c, f.Namespace).List(ctx, client.ListOptions{})
		framework.ExpectNoError(err)
		_, err = client.Secrets(c, f.Namespace).List(ctx, client.ListOptions{})
		Expect(client.IsForbidden(err)).To(BeTrue(), "%v", err)

		By("a token bound to a Pod stops working once the Pod is gone")
		f.Apply(ctx, sleeper("holder", map[string]any{"serviceAccountName": "batch", "terminationGracePeriodSeconds": 1}))
		pod := f.WaitForRunningPods(ctx, map[string]string{"app": "holder"}, 1)[0]
		var req authenticationv1.TokenRequest
		req.Spec.BoundObjectRef = &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID}
		framework.ExpectNoError(f.Client.Do(ctx, http.MethodPost, "/api/v1/namespaces/"+f.Namespace+"/serviceaccounts/batch/token",
			nil, "", &req, &req))
		bound := client.New(framework.TestContext.APIURL, req.Status.Token)
		_, err = client.Pods(bound, f.Namespace).List(ctx, client.ListOptions{})
		framework.ExpectNoError(err)
		f.KK(ctx, "delete", "pod", pod.Name)
		Eventually(ctx, func() bool {
			_, err := client.Pods(bound, f.Namespace).List(ctx, client.ListOptions{})
			return client.Reason(err) == "Unauthorized"
		}).WithTimeout(time.Minute).Should(BeTrue())

		By("deleting the service account invalidates its tokens")
		f.KK(ctx, "delete", "serviceaccount", "batch")
		_, err = client.Pods(c, f.Namespace).List(ctx, client.ListOptions{})
		Expect(client.Reason(err)).To(BeEquivalentTo("Unauthorized"))
	})

	It("should refuse Pods of missing service accounts and leave out tokens on request", func(ctx SpecContext) {
		out := f.ApplyExpectError(ctx, sleeper("ghost", map[string]any{"serviceAccountName": "ghost"}))
		Expect(out).To(ContainSubstring(`pods "ghost" is forbidden: error looking up service account ` + f.Namespace + `/ghost: serviceaccount "ghost" not found`))
		f.Apply(ctx, sleeper("bare", map[string]any{"automountServiceAccountToken": false}))
		pod := f.WaitForRunningPods(ctx, map[string]string{"app": "bare"}, 1)[0]
		Expect(pod.Spec.Volumes).To(BeEmpty())
		_, err := framework.RunKK(ctx, "exec", "-n", f.Namespace, "bare", "--", "/mini", "ls", "/var/run/secrets/kubernetes.io/serviceaccount")
		Expect(err).To(HaveOccurred())
	})
})
