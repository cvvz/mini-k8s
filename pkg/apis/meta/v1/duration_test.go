package v1

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDurationJSON(t *testing.T) {
	data, err := json.Marshal(Duration{60348 * time.Millisecond})
	if err != nil || string(data) != `"1m0.348s"` {
		t.Fatalf("marshal: %s %v", data, err)
	}
	var d Duration
	if err := json.Unmarshal([]byte(`"55.932s"`), &d); err != nil || d.Duration != 55932*time.Millisecond {
		t.Fatalf("unmarshal: %v %v", d, err)
	}
	if err := json.Unmarshal([]byte(`"soon"`), &d); err == nil {
		t.Fatal("not a duration")
	}
}
