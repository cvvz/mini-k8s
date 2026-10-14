package scheduler

import "testing"

func TestUnschedulableMessageLikeKubernetes(t *testing.T) {
	got := unschedulableMessage(3, map[string]int{
		"were unschedulable": 2,
		"had untolerated taint {node-role.kubernetes.io/control-plane: }": 1,
	})
	want := "0/3 nodes are available: 1 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }, 2 node(s) were unschedulable."
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := unschedulableMessage(0, nil); got != "0/0 nodes are available." {
		t.Fatalf("no nodes: %q", got)
	}
}
