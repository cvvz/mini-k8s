package v1

import (
	"testing"

	"mini-k8s/pkg/labels"
)

func TestLabelSelectorAsSelector(t *testing.T) {
	ls := &LabelSelector{
		MatchLabels:      map[string]string{"app": "web"},
		MatchExpressions: []LabelSelectorRequirement{{Key: "env", Operator: labels.In, Values: []string{"a", "b"}}},
	}
	sel, err := ls.AsSelector()
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches(map[string]string{"app": "web", "env": "b"}) || sel.Matches(map[string]string{"app": "web", "env": "c"}) {
		t.Fatal("wrong matching")
	}
	if got := sel.String(); got != "app=web,env in (a,b)" {
		t.Fatalf("String() = %q", got)
	}
	if _, err := (&LabelSelector{MatchExpressions: []LabelSelectorRequirement{{Key: "a", Operator: "Gt"}}}).AsSelector(); err == nil {
		t.Fatal("unsupported operator accepted")
	}
}

func TestFinalizerHelpers(t *testing.T) {
	m := &ObjectMeta{Finalizers: []string{"a", "b"}}
	if !m.HasFinalizer("a") || !m.RemoveFinalizer("a") || m.HasFinalizer("a") || m.RemoveFinalizer("a") {
		t.Fatal("finalizer helpers broken")
	}
	m.RemoveFinalizer("b")
	if m.Finalizers != nil {
		t.Fatal("empty finalizers should be nil")
	}
}
