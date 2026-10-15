package plugins

import (
	"testing"

	"mini-k8s/pkg/api/resource"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/scheduler/framework"
)

func node(name string, cpu, mem string, labels map[string]string, taints ...corev1.Taint) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Spec: corev1.NodeSpec{Taints: taints}}
	n.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem), corev1.ResourcePods: resource.MustParse("110")}
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	n.Labels[corev1.LabelHostname] = name
	return n
}

func pod(name string, mutate func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: name, Labels: map[string]string{}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func requests(cpu, mem string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)}
	}
}

func on(nodeName string, mutate func(*corev1.Pod)) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Spec.NodeName = nodeName
		if mutate != nil {
			mutate(p)
		}
	}
}

func schedule(t *testing.T, p *corev1.Pod, nodes []*corev1.Node, pods ...*corev1.Pod) (string, string) {
	t.Helper()
	f := Default()
	f.Rand = func(int) int { return 0 }
	return f.Schedule(p, framework.NewSnapshot(nodes, pods))
}

func TestFiltersAndMessages(t *testing.T) {
	cp := node("control-plane", "2", "4Gi", nil, corev1.Taint{Key: corev1.TaintNodeRoleControlPlane, Effect: corev1.TaintEffectNoSchedule})
	w1 := node("worker-1", "2", "4Gi", map[string]string{"disk": "ssd"})
	w2 := node("worker-2", "2", "4Gi", map[string]string{"disk": "hdd"})
	nodes := []*corev1.Node{cp, w1, w2}

	if got, _ := schedule(t, pod("p", func(p *corev1.Pod) { p.Spec.NodeSelector = map[string]string{"disk": "hdd"} }), nodes); got != "worker-2" {
		t.Errorf("nodeSelector: %s", got)
	}
	_, msg := schedule(t, pod("p", func(p *corev1.Pod) { p.Spec.NodeSelector = map[string]string{"disk": "nvme"} }), nodes)
	if want := "0/3 nodes are available: 1 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }, 2 node(s) didn't match Pod's node affinity/selector."; msg != want {
		t.Errorf("got  %q\nwant %q", msg, want)
	}
	_, msg = schedule(t, pod("big", requests("3", "5Gi")), nodes)
	if want := "0/3 nodes are available: 1 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }, 2 Insufficient cpu, 2 Insufficient memory."; msg != want {
		t.Errorf("got  %q\nwant %q", msg, want)
	}
	// Requests of placed Pods count, init containers by their maximum.
	busy := pod("busy", on("worker-1", requests("1500m", "1Gi")))
	initHeavy := pod("init", func(p *corev1.Pod) {
		requests("1", "1Gi")(p)
		p.Spec.InitContainers = []corev1.Container{{Name: "i", Image: "img", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1800m")}}}}
	})
	if got, msg := schedule(t, initHeavy, nodes, busy); got != "worker-2" {
		t.Errorf("init containers count by maximum: %s %s", got, msg)
	}
	w1.Spec.Unschedulable = true
	if got, _ := schedule(t, pod("p", nil), []*corev1.Node{w1}); got != "" {
		t.Error("cordoned node")
	}
	tolerant := pod("p", func(p *corev1.Pod) {
		p.Spec.Tolerations = []corev1.Toleration{{Key: "node.kubernetes.io/unschedulable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
	})
	if got, _ := schedule(t, tolerant, []*corev1.Node{w1}); got != "worker-1" {
		t.Error("a Pod tolerating unschedulable may use a cordoned node")
	}
}

func TestScoring(t *testing.T) {
	w1 := node("worker-1", "4", "8Gi", map[string]string{"zone": "a"})
	w2 := node("worker-2", "4", "8Gi", map[string]string{"zone": "b"})
	nodes := []*corev1.Node{w1, w2}
	owned := func(p *corev1.Pod) {
		yes := true
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web", UID: "rs-1", Controller: &yes}}
		p.Labels["app"] = "web"
	}
	// Replicas spread even though worker-2 is busier with other Pods.
	sibling := pod("web-1", on("worker-1", owned))
	others := []*corev1.Pod{sibling, pod("x1", on("worker-2", nil)), pod("x2", on("worker-2", nil))}
	if got, _ := schedule(t, pod("web-2", owned), nodes, others...); got != "worker-2" {
		t.Errorf("spreading: %s", got)
	}
	// Less allocated nodes win for unrelated Pods.
	if got, _ := schedule(t, pod("lone", nil), nodes, pod("heavy", on("worker-1", requests("3", "6Gi")))); got != "worker-2" {
		t.Errorf("least allocated: %s", got)
	}
	// Preferred node affinity steers.
	prefer := pod("p", func(p *corev1.Pod) {
		p.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{Preferred: []corev1.PreferredSchedulingTerm{{Weight: 50,
			Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: "In", Values: []string{"b"}}}}}}}}
	})
	if got, _ := schedule(t, prefer, nodes); got != "worker-2" {
		t.Errorf("preferred node affinity: %s", got)
	}
	// PreferNoSchedule taints repel.
	soft := node("worker-1", "4", "8Gi", nil, corev1.Taint{Key: "spot", Effect: corev1.TaintEffectPreferNoSchedule})
	if got, _ := schedule(t, pod("p", nil), []*corev1.Node{soft, w2}); got != "worker-2" {
		t.Errorf("PreferNoSchedule: %s", got)
	}
}

func TestInterPodAffinity(t *testing.T) {
	w1 := node("worker-1", "4", "8Gi", nil)
	w2 := node("worker-2", "4", "8Gi", nil)
	nodes := []*corev1.Node{w1, w2}
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}
	term := corev1.PodAffinityTerm{LabelSelector: selector, TopologyKey: corev1.LabelHostname}
	db := func(name, nodeName string) *corev1.Pod {
		return pod(name, func(p *corev1.Pod) {
			p.Labels["app"] = "db"
			p.Spec.NodeName = nodeName
			p.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{Required: []corev1.PodAffinityTerm{term}}}
		})
	}
	if got, _ := schedule(t, db("db-1", ""), nodes, db("db-0", "worker-1")); got != "worker-2" {
		t.Errorf("anti-affinity: %s", got)
	}
	_, msg := schedule(t, db("db-2", ""), nodes, db("db-0", "worker-1"), db("db-1", "worker-2"))
	if want := "0/2 nodes are available: 2 node(s) didn't satisfy existing pods anti-affinity rules."; msg != want {
		t.Errorf("got  %q\nwant %q", msg, want)
	}
	// Affinity: next to a db Pod; the first Pod of a self-selecting group
	// may go anywhere.
	cache := pod("cache", func(p *corev1.Pod) {
		p.Spec.Affinity = &corev1.Affinity{PodAffinity: &corev1.PodAffinity{Required: []corev1.PodAffinityTerm{term}}}
	})
	if got, _ := schedule(t, cache, nodes, pod("db-0", on("worker-2", func(p *corev1.Pod) { p.Labels["app"] = "db" }))); got != "worker-2" {
		t.Errorf("affinity: %s", got)
	}
	if _, msg := schedule(t, cache, nodes); msg != "0/2 nodes are available: 2 node(s) didn't match pod affinity rules." {
		t.Errorf("no db Pod: %q", msg)
	}
	first := pod("db-first", func(p *corev1.Pod) {
		p.Labels["app"] = "db"
		p.Spec.Affinity = &corev1.Affinity{PodAffinity: &corev1.PodAffinity{Required: []corev1.PodAffinityTerm{term}}}
	})
	if got, _ := schedule(t, first, nodes); got == "" {
		t.Error("the first Pod of a group selecting itself is schedulable")
	}
}
