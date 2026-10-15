package daemon

import (
	"strings"
	"testing"
	"time"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/util/intstr"
)

func daemonSet(mutate func(*corev1.PodSpec)) *appsv1.DaemonSet {
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns", UID: "ds-uid",
		Annotations: map[string]string{appsv1.DeprecatedTemplateGeneration: "2"}},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "agent"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}}}
	if mutate != nil {
		mutate(&ds.Spec.Template.Spec)
	}
	return ds
}

func testNode(name string, labels map[string]string, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Spec: corev1.NodeSpec{Taints: taints}}
}

func TestShouldRunOn(t *testing.T) {
	plain := daemonSet(nil)
	selective := daemonSet(func(s *corev1.PodSpec) { s.NodeSelector = map[string]string{"disk": "ssd"} })
	tolerant := daemonSet(func(s *corev1.PodSpec) { s.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}} })
	noSchedule := corev1.Taint{Key: "dedicated", Value: "x", Effect: corev1.TaintEffectNoSchedule}
	noExecute := corev1.Taint{Key: "dedicated", Value: "x", Effect: corev1.TaintEffectNoExecute}
	cordoned := testNode("cordoned", nil)
	cordoned.Spec.Unschedulable = true
	for _, tc := range []struct {
		ds                 *appsv1.DaemonSet
		node               *corev1.Node
		run, keepOnRunning bool
	}{
		{plain, testNode("n", nil), true, true},
		{selective, testNode("n", nil), false, false},
		{selective, testNode("n", map[string]string{"disk": "ssd"}), true, true},
		{plain, testNode("n", nil, noSchedule), false, true},
		{plain, testNode("n", nil, noExecute), false, false},
		{tolerant, testNode("n", nil, noExecute), true, true},
		{plain, testNode("n", nil, corev1.Taint{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoExecute}), true, true},
		{plain, testNode("n", nil, corev1.Taint{Key: "soft", Effect: corev1.TaintEffectPreferNoSchedule}), true, true},
		{plain, cordoned, true, true},
	} {
		run, keep := ShouldRunOn(tc.ds, tc.node)
		if run != tc.run || keep != tc.keepOnRunning {
			t.Errorf("%v on %v: got %v %v, want %v %v", tc.ds.Spec.Template.Spec.NodeSelector, tc.node.Spec, run, keep, tc.run, tc.keepOnRunning)
		}
	}
}

func TestNewPod(t *testing.T) {
	ds := daemonSet(func(s *corev1.PodSpec) { s.HostNetwork = true })
	p := NewPod(ds, "worker-1", "abc")
	if p.GenerateName != "agent-" || p.Labels["app"] != "agent" || p.Labels[appsv1.ControllerRevisionHashLabelKey] != "abc" ||
		p.Labels[appsv1.DaemonSetTemplateGenerationKey] != "2" || metav1.ControllerOf(&p.ObjectMeta).UID != "ds-uid" {
		t.Fatalf("metadata: %+v", p.ObjectMeta)
	}
	if podNode(p) != "worker-1" || p.Spec.NodeName != "" {
		t.Fatalf("the Pod is pinned by affinity, not bound: %+v", p.Spec.Affinity)
	}
	var keys []string
	for _, tol := range p.Spec.Tolerations {
		keys = append(keys, tol.Key+":"+tol.Effect)
	}
	if want := "node.kubernetes.io/not-ready:NoExecute,node.kubernetes.io/unreachable:NoExecute,node.kubernetes.io/disk-pressure:NoSchedule," +
		"node.kubernetes.io/memory-pressure:NoSchedule,node.kubernetes.io/pid-pressure:NoSchedule,node.kubernetes.io/unschedulable:NoSchedule," +
		"node.kubernetes.io/network-unavailable:NoSchedule"; strings.Join(keys, ",") != want {
		t.Fatalf("tolerations: %v", keys)
	}
	if !corev1.MatchesNodeSelector(&p.Spec, testNode("worker-1", nil)) || corev1.MatchesNodeSelector(&p.Spec, testNode("worker-2", nil)) {
		t.Fatal("the affinity selects the node by name")
	}
	if len(ds.Spec.Template.Spec.Tolerations) != 0 {
		t.Fatal("the template must not change")
	}
}

// A node with an old and a new Pod keeps both only while a surge is
// allowed; otherwise the newer one goes (say, after maxSurge was set back
// to 0 mid-rollout).
func TestManageKeepsOnePodUnlessSurging(t *testing.T) {
	dc := &Controller{failed: map[string]failedPods{}, now: time.Now}
	at := func(sec int64) metav1.Time { return metav1.NewTime(time.Unix(sec, 0)) }
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", CreationTimestamp: at(1),
		Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "v1"}}, Spec: corev1.PodSpec{NodeName: "n1"}}
	newPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "new", CreationTimestamp: at(2),
		Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "v2"}}, Spec: corev1.PodSpec{NodeName: "n1"}}
	byNode := map[string][]*corev1.Pod{"n1": {oldPod, newPod}}
	nodes := []*corev1.Node{testNode("n1", nil)}
	for surge, want := range map[int]string{0: "new", 1: ""} {
		ds := daemonSet(nil)
		ms, mu := intstr.FromInt(surge), intstr.FromInt(1-surge)
		ds.Spec.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{Type: appsv1.RollingUpdateDaemonSetStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxSurge: &ms, MaxUnavailable: &mu}}
		creates, deletes := dc.manage(ds, nodes, byNode, "v2")
		got := ""
		for _, p := range deletes {
			got += p.Name
		}
		if len(creates) != 0 || got != want {
			t.Errorf("maxSurge %d: creates %v, deletes %q, want %q", surge, creates, got, want)
		}
	}
}

func TestPlanRollingUpdate(t *testing.T) {
	pod := func(name, hash string, ready bool) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: hash}},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: map[bool]string{true: "True", false: "False"}[ready]}}}}
	}
	available := func(p *corev1.Pod) bool { return corev1.IsPodReady(p) }
	names := func(pods []*corev1.Pod) string {
		var out []string
		for _, p := range pods {
			out = append(out, p.Name)
		}
		return strings.Join(out, ",")
	}
	// Three nodes on the old revision: one at a time.
	old := []nodePods{{"a", []*corev1.Pod{pod("a-old", "v1", true)}}, {"b", []*corev1.Pod{pod("b-old", "v1", true)}},
		{"c", []*corev1.Pod{pod("c-old", "v1", false)}}}
	deletes, surge := planRollingUpdate(old, "v2", 1, 0, available)
	if names(deletes) != "c-old,a-old" || surge != nil {
		t.Errorf("unavailable old Pods go at once plus one more: %s %v", names(deletes), surge)
	}
	// One node already updating (no Pod there yet): no budget left.
	inFlight := []nodePods{{"a", nil}, {"b", []*corev1.Pod{pod("b-old", "v1", true)}}}
	if deletes, _ := planRollingUpdate(inFlight, "v2", 1, 0, available); len(deletes) != 0 {
		t.Errorf("a node without a Pod uses the budget: %s", names(deletes))
	}
	// A new Pod that is not available yet also uses the budget.
	starting := []nodePods{{"a", []*corev1.Pod{pod("a-new", "v2", false)}}, {"b", []*corev1.Pod{pod("b-old", "v1", true)}}}
	if deletes, _ := planRollingUpdate(starting, "v2", 1, 0, available); len(deletes) != 0 {
		t.Errorf("an unavailable new Pod uses the budget: %s", names(deletes))
	}
	if deletes, _ := planRollingUpdate(starting, "v2", 2, 0, available); names(deletes) != "b-old" {
		t.Errorf("maxUnavailable 2: %s", names(deletes))
	}

	// Surge: new Pods start next to old ones, old ones go once replaced.
	deletes, surge = planRollingUpdate(old, "v2", 0, 1, available)
	if len(deletes) != 0 || strings.Join(surge, ",") != "c,a" {
		t.Errorf("surge onto the unavailable node and one more: %s %v", names(deletes), surge)
	}
	surging := []nodePods{{"a", []*corev1.Pod{pod("a-old", "v1", true), pod("a-new", "v2", true)}},
		{"b", []*corev1.Pod{pod("b-old", "v1", true), pod("b-new", "v2", false)}}, {"c", []*corev1.Pod{pod("c-old", "v1", true)}}}
	deletes, surge = planRollingUpdate(surging, "v2", 0, 1, available)
	if names(deletes) != "a-old" || len(surge) != 0 {
		t.Errorf("replace a; b still surging: %s %v", names(deletes), surge)
	}
}
