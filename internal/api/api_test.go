package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseSelectorMatches(t *testing.T) {
	labels := map[string]string{"app": "web", "tier": "front", "env": "prod"}
	for _, tc := range []struct {
		sel  string
		want bool
	}{
		{"", true},
		{"app=web", true},
		{"app==web,tier=front", true},
		{"app!=web", false},
		{"app!=db", true},
		{"env in (prod,staging)", true},
		{"env notin (prod)", false},
		{"tier,app=web", true},
		{"!missing", true},
		{"!app", false},
		{"missing", false},
		{"app=web, env in (dev, prod), !canary", true},
	} {
		sel, err := ParseSelector(tc.sel)
		if err != nil {
			t.Fatalf("%q: %v", tc.sel, err)
		}
		if got := sel.Matches(labels); got != tc.want {
			t.Errorf("%q matched=%v want %v", tc.sel, got, tc.want)
		}
	}
	for _, bad := range []string{"app in prod", "app in ()", "a b c", "app=we b", "-bad=x"} {
		if _, err := ParseSelector(bad); err == nil {
			t.Errorf("%q: expected parse error", bad)
		}
	}
}

func TestLabelSelectorAsSelector(t *testing.T) {
	ls := &LabelSelector{
		MatchLabels:      map[string]string{"app": "web"},
		MatchExpressions: []LabelSelectorRequirement{{Key: "env", Operator: OpIn, Values: []string{"a", "b"}}},
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

func TestFieldSelector(t *testing.T) {
	sel, err := ParseFieldSelector("spec.nodeName=worker-1,status.phase!=Failed")
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches(map[string]string{"spec.nodeName": "worker-1", "status.phase": "Running"}) {
		t.Fatal("should match")
	}
	if sel.Matches(map[string]string{"spec.nodeName": "worker-2", "status.phase": "Running"}) {
		t.Fatal("should not match")
	}
	if err := sel.Validate(map[string]string{"spec.nodeName": ""}); err == nil || !strings.Contains(err.Error(), "status.phase") {
		t.Fatalf("unsupported field not reported: %v", err)
	}
}

func TestObjectJSONShape(t *testing.T) {
	p := &Pod{ObjectMeta: ObjectMeta{Name: "a", Namespace: "default"}, Spec: PodSpec{Containers: []Container{{Name: "c", Image: "i"}}}}
	Pods.SetTypeMeta(p)
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{`"apiVersion":"v1"`, `"kind":"Pod"`, `"metadata":{"name":"a","namespace":"default"}`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	if strings.Contains(s, "status") || strings.Contains(s, "creationTimestamp") {
		t.Errorf("zero status/timestamps must be omitted: %s", s)
	}
	now := NewTime(time.Date(2026, 10, 14, 1, 2, 3, 999, time.UTC))
	p.CreationTimestamp = now
	data, _ = json.Marshal(p)
	var back Pod
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !back.CreationTimestamp.Equal(now.Time) || !strings.Contains(string(data), `"2026-10-14T01:02:03Z"`) {
		t.Fatalf("time round trip: %s", data)
	}
}

func TestLookupAndPaths(t *testing.T) {
	for _, name := range []string{"po", "pods", "pod", "Pod"} {
		if r, ok := Lookup(name); !ok || r.Kind != "Pod" {
			t.Fatalf("Lookup(%q) failed", name)
		}
	}
	if r, ok := Lookup("deployments.apps"); !ok || r.Kind != "Deployment" {
		t.Fatal("group-qualified lookup failed")
	}
	if got := Deployments.Path("ns1", "web", "status"); got != "/apis/apps/v1/namespaces/ns1/deployments/web/status" {
		t.Fatal(got)
	}
	if got := Nodes.Path("ignored", "n1"); got != "/api/v1/nodes/n1" {
		t.Fatal(got)
	}
	if got := Pods.Path("", ""); got != "/api/v1/pods" {
		t.Fatal(got)
	}
	if r, ok := ForKind("apps/v1", "Deployment"); !ok || r.Resource != "deployments" {
		t.Fatal("ForKind failed")
	}
	if _, ok := ForKind("v1", "Deployment"); ok {
		t.Fatal("wrong group accepted")
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
