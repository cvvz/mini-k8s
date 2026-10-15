package nodelifecycle

import (
	"testing"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
)

func TestConditionTaints(t *testing.T) {
	now := metav1.NewTime(time.Unix(1000, 0))
	role := corev1.Taint{Key: corev1.TaintNodeRoleControlPlane, Effect: corev1.TaintEffectNoSchedule}
	unknown := &corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}
	notReady := &corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse}
	ready := &corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}

	taints, changed := conditionTaints([]corev1.Taint{role}, unknown, now)
	if !changed || len(taints) != 3 || taints[1].Key != corev1.TaintNodeUnreachable || taints[1].Effect != corev1.TaintEffectNoSchedule ||
		taints[2].Effect != corev1.TaintEffectNoExecute || !taints[2].TimeAdded.Time.Equal(now.Time) {
		t.Fatalf("unreachable: %+v", taints)
	}
	later := metav1.NewTime(now.Add(time.Minute))
	if again, changed := conditionTaints(taints, unknown, later); changed || !again[2].TimeAdded.Time.Equal(now.Time) {
		t.Fatalf("already tainted: %v %+v", changed, again)
	}
	swapped, changed := conditionTaints(taints, notReady, later)
	if !changed || len(swapped) != 3 || swapped[1].Key != corev1.TaintNodeNotReady || swapped[2].Key != corev1.TaintNodeNotReady {
		t.Fatalf("not ready: %+v", swapped)
	}
	if cleared, changed := conditionTaints(swapped, ready, later); !changed || len(cleared) != 1 || cleared[0] != role {
		t.Fatalf("ready again: %+v", cleared)
	}
	if _, changed := conditionTaints(nil, ready, later); changed {
		t.Fatal("a ready node without taints is left alone")
	}
}

func TestMarkUnknown(t *testing.T) {
	now := metav1.NewTime(time.Unix(1000, 0))
	n := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"}}}}
	markUnknown(n, now)
	c := n.Status.Condition(corev1.NodeReady)
	if c.Status != corev1.ConditionUnknown || c.Reason != "NodeStatusUnknown" || c.Message != "Kubelet stopped posting node status." || !c.LastTransitionTime.Time.Equal(now.Time) {
		t.Fatalf("%+v", c)
	}
	fresh := &corev1.Node{}
	markUnknown(fresh, now)
	if c := fresh.Status.Condition(corev1.NodeReady); c == nil || c.Reason != "NodeStatusNeverUpdated" {
		t.Fatalf("never posted: %+v", fresh.Status)
	}
}
