package node

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/test/e2e/framework"
)

var _ = framework.SIGDescribe("node", "Volumes and host network", func() {
	f := framework.NewDefaultFramework("volumes")

	kkExec := func(ctx SpecContext, pod string, args ...string) (string, error) {
		return framework.RunKK(ctx, append([]string{"exec", "-n", f.Namespace, pod, "--"}, args...)...)
	}
	mount := func(c framework.Container, mounts ...map[string]any) framework.Container {
		list := make([]any, len(mounts))
		for i, m := range mounts {
			list[i] = m
		}
		c.Fields = map[string]any{"volumeMounts": list}
		return c
	}
	withVolumes := func(o framework.Object, vols ...map[string]any) framework.Object {
		list := make([]any, len(vols))
		for i, v := range vols {
			list[i] = v
		}
		o.PodSpec()["volumes"] = list
		return o
	}
	nodePodDirs := func(ctx SpecContext, node string) string {
		out, err := framework.Docker(ctx, "exec", framework.ComponentContainer(node), "sh", "-c", "ls /var/lib/kubelet/pods 2>/dev/null || true")
		framework.ExpectNoError(err)
		return out
	}

	It("should share an emptyDir between an init container and the app, and remove it with the Pod", func(ctx SpecContext) {
		work := map[string]any{"name": "work", "mountPath": "/work"}
		cache := map[string]any{"name": "cache", "mountPath": "/cache"}
		obj := framework.NewPod("shared", map[string]string{"app": "shared"}, mount(framework.DemoContainer(), work, cache)).
			WithInitContainers(mount(framework.MiniContainer("prepare", "write", "/work/greeting", "hello from init"), work))
		withVolumes(obj,
			map[string]any{"name": "work", "emptyDir": map[string]any{}},
			map[string]any{"name": "cache", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "8Mi"}})
		f.Apply(ctx, obj)
		pod := f.WaitForRunningPods(ctx, map[string]string{"app": "shared"}, 1)[0]

		Expect(kkExec(ctx, "shared", "/mini", "cat", "/work/greeting")).To(Equal("hello from init\n"))
		mounts, err := kkExec(ctx, "shared", "/mini", "cat", "/proc/mounts")
		framework.ExpectNoError(err)
		Expect(mounts).To(MatchRegexp(`(?m)^tmpfs /cache tmpfs .*size=8192k`), "a Memory emptyDir is a tmpfs of its size limit")
		Expect(mounts).NotTo(MatchRegexp(`(?m)^tmpfs /work `), "a disk emptyDir lives on the node's disk")
		Expect(nodePodDirs(ctx, pod.Spec.NodeName)).To(ContainSubstring(pod.UID))

		f.KK(ctx, "delete", "pod", "shared")
		f.WaitForPodGone(ctx, &pod, 30*time.Second)
		Eventually(ctx, func() string { return nodePodDirs(ctx, pod.Spec.NodeName) }).
			WithTimeout(15*time.Second).ShouldNot(ContainSubstring(pod.UID), "the Pod's volumes are deleted with it")
	})

	It("should mount hostPath volumes read-write and read-only", func(ctx SpecContext) {
		dir := "/var/lib/mini-e2e/" + f.Namespace
		DeferCleanup(func(ctx SpecContext) {
			_, _ = framework.Docker(ctx, "exec", framework.ComponentContainer("worker-1"), "rm", "-rf", dir)
		})
		c := mount(framework.DemoContainer(),
			map[string]any{"name": "host", "mountPath": "/host"},
			map[string]any{"name": "host", "mountPath": "/ro", "readOnly": true})
		obj := withVolumes(framework.NewPod("hostdir", map[string]string{"app": "hostdir"}, c),
			map[string]any{"name": "host", "hostPath": map[string]any{"path": dir, "type": "DirectoryOrCreate"}})
		obj.Spec()["nodeName"] = "worker-1"
		f.Apply(ctx, obj)
		f.WaitForRunningPods(ctx, map[string]string{"app": "hostdir"}, 1)

		_, err := kkExec(ctx, "hostdir", "/mini", "write", "/host/marker", "written by a Pod")
		framework.ExpectNoError(err)
		Expect(framework.Docker(ctx, "exec", framework.ComponentContainer("worker-1"), "cat", dir+"/marker")).To(Equal("written by a Pod\n"))
		Expect(kkExec(ctx, "hostdir", "/mini", "cat", "/ro/marker")).To(Equal("written by a Pod\n"))
		out, err := kkExec(ctx, "hostdir", "/mini", "write", "/ro/other", "x")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("read-only file system"))
	})

	It("should refuse to start containers when a hostPath type check fails", func(ctx SpecContext) {
		missing := "/var/lib/mini-e2e-missing/" + f.Namespace
		obj := withVolumes(framework.NewPod("nodir", map[string]string{"app": "nodir"},
			mount(framework.DemoContainer(), map[string]any{"name": "host", "mountPath": "/host"})),
			map[string]any{"name": "host", "hostPath": map[string]any{"path": missing, "type": "Directory"}})
		f.Apply(ctx, obj)
		pod := f.WaitForPod(ctx, "nodir", "reports the volume error", func(p *corev1.Pod) bool {
			cs := p.Status.ContainerStatuses
			return len(cs) == 1 && cs[0].State.Waiting != nil && cs[0].State.Waiting.Reason == "CreateContainerConfigError"
		})
		Expect(pod.Status.ContainerStatuses[0].State.Waiting.Message).To(Equal(
			`MountVolume.SetUp failed for volume "host": hostPath type check failed: ` + missing + ` is not a directory`))
		Expect(f.KK(ctx, "get", "pod", "nodir")).To(MatchRegexp(`nodir\s+0/1\s+CreateContainerConfigError`))
		f.WaitForEvent(ctx, "nodir", "Failed")
	})

	It("should run hostNetwork Pods in the node's network namespace", func(ctx SpecContext) {
		web := framework.DemoContainer()
		web.Env = map[string]string{"DEMO_PORT": "18081"}
		host := framework.NewPod("hostnet", map[string]string{"app": "hostnet"}, web)
		host.Spec()["nodeName"], host.Spec()["hostNetwork"] = "worker-1", true
		client := framework.NewPod("caller", map[string]string{"app": "caller"}, framework.DemoContainer())
		client.Spec()["nodeName"] = "worker-2"
		f.Apply(ctx, host, client)
		pod := f.WaitForRunningPods(ctx, map[string]string{"app": "hostnet"}, 1)[0]
		f.WaitForRunningPods(ctx, map[string]string{"app": "caller"}, 1)

		Expect(pod.Status.PodIP).To(Equal(pod.Status.HostIP), "a hostNetwork Pod has the node's address")
		Expect(strings.HasPrefix(pod.Status.PodIP, "10.244.")).To(BeFalse())
		Eventually(ctx, func() (string, error) {
			return kkExec(ctx, "caller", "/mini", "fetch", "http://"+pod.Status.HostIP+":18081/")
		}).WithTimeout(20*time.Second).Should(Equal("200\nhello from worker-1\n"), "the Pod listens on the node and has its hostname")
	})
})
