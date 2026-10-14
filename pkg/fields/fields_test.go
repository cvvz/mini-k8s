package fields

import (
	"strings"
	"testing"
)

func TestFieldSelector(t *testing.T) {
	sel, err := ParseSelector("spec.nodeName=worker-1,status.phase!=Failed")
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
