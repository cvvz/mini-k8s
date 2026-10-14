package mini

import (
	"encoding/json"
	"testing"
	"time"
)

func TestValidation(t *testing.T) {
	valid := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"web"},"spec":{"containers":[{"name":"web","image":"mini-k8s:local"}]}}`
	for _, tc := range []struct {
		name, data string
		ok         bool
	}{
		{"valid", valid, true},
		{"unknown field", `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"x"},"spec":{"volumes":[]}}`, false},
		{"bad name", `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"../x"},"spec":{"containers":[{"name":"x","image":"x"}]}}`, false},
		{"unsupported namespace", `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"x","namespace":"prod"},"spec":{"containers":[{"name":"x","image":"x"}]}}`, false},
		{"negative replicas", `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web"},"spec":{"replicas":-1,"selector":{"matchLabels":{"app":"web"}},"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"web","image":"x"}]}}}}`, false},
		{"wrong version", `{"apiVersion":"apps/v1","kind":"Pod","metadata":{"name":"x"},"spec":{"containers":[{"name":"x","image":"x"}]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode([]byte(tc.data))
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, err)
			}
		})
	}
}

func TestSchedulerIgnoresStaleNodesAndBalancesAssignedPods(t *testing.T) {
	now := time.Now()
	nodes := []Resource{
		{Metadata: Metadata{Name: "a"}, Status: Status{Heartbeat: now, Phase: "Ready"}},
		{Metadata: Metadata{Name: "b"}, Status: Status{Heartbeat: now, Phase: "Ready"}},
		{Metadata: Metadata{Name: "stale"}, Status: Status{Heartbeat: now.Add(-time.Minute), Phase: "Ready"}},
	}
	pods := []Resource{{Status: Status{NodeName: "a"}}}
	if got := ChooseNode(nodes, pods, now); got != "b" {
		t.Fatalf("got %q want b", got)
	}
	if got := ChooseNode(nodes[2:], nil, now); got != "" {
		t.Fatalf("scheduled stale node: %q", got)
	}
}

func TestDeploymentOwnershipNotJustLabels(t *testing.T) {
	d := Resource{Metadata: Metadata{UID: "new"}}
	pods := []Resource{
		{Metadata: Metadata{Name: "old", OwnerUID: "old"}},
		{Metadata: Metadata{Name: "mine", OwnerUID: "new"}},
		{Metadata: Metadata{Name: "standalone"}},
	}
	got := OwnedPods(d, pods)
	if len(got) != 1 || got[0].Metadata.Name != "mine" {
		t.Fatalf("got %#v", got)
	}
}

func TestOnlyReplicaUpdatesAllowed(t *testing.T) {
	old := Resource{Kind: "Deployment", Spec: json.RawMessage(`{"replicas":2,"selector":{"matchLabels":{"app":"web"}},"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"web","image":"a"}]}}}`)}
	next := old
	next.Spec = json.RawMessage(`{"replicas":3,"selector":{"matchLabels":{"app":"web"}},"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"web","image":"a"}]}}}`)
	if err := CheckUpdate(old, next); err != nil {
		t.Fatal(err)
	}
	next.Spec = json.RawMessage(`{"replicas":3,"selector":{"matchLabels":{"app":"web"}},"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"web","image":"b"}]}}}`)
	if err := CheckUpdate(old, next); err == nil {
		t.Fatal("allowed template mutation")
	}
}
