package resource

import (
	"encoding/json"
	"testing"
)

func TestParseQuantity(t *testing.T) {
	for in, milli := range map[string]int64{
		"2": 2000, "0.5": 500, "250m": 250, "1.5": 1500, "0.0001": 1, // rounds up to 1m
		"1k": 1e6, "500M": 5e11, "1G": 1e12,
		"1Ki": 1024 * 1000, "128Mi": 128 << 20 * 1000, "1.5Gi": 3 << 29 * 1000,
	} {
		q, err := ParseQuantity(in)
		if err != nil || q.MilliValue() != milli || q.String() != in {
			t.Errorf("%s: %d %q %v, want %d", in, q.MilliValue(), q.String(), err, milli)
		}
	}
	for _, in := range []string{"", "-1", "abc", "1.2.3", "1Xi", "1 Gi", ".", "10Pi0"} {
		if _, err := ParseQuantity(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	if _, err := ParseQuantity("100000Pi"); err == nil {
		t.Error("overflow accepted")
	}
}

func TestQuantityHelpers(t *testing.T) {
	if MustParse("1").Cmp(MustParse("1000m")) != 0 || MustParse("1Gi").Cmp(MustParse("1G")) != 1 || MustParse("100m").Cmp(MustParse("1")) != -1 {
		t.Error("Cmp")
	}
	if MustParse("1500m").Value() != 2 || MustParse("128Mi").Value() != 128<<20 {
		t.Error("Value rounds up to whole units")
	}
	if NewMilliQuantity(500).String() != "500m" || NewMilliQuantity(2000).String() != "2" || NewQuantity(1024).String() != "1024" {
		t.Error("formatting")
	}
	if !(Quantity{}).IsZero() || (Quantity{}).String() != "0" {
		t.Error("zero value")
	}
}

func TestQuantityJSON(t *testing.T) {
	var v struct {
		CPU    Quantity `json:"cpu"`
		Memory Quantity `json:"memory"`
	}
	if err := json.Unmarshal([]byte(`{"cpu": 0.5, "memory": "64Mi"}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.CPU.MilliValue() != 500 || v.Memory.Value() != 64<<20 {
		t.Fatalf("%+v", v)
	}
	out, _ := json.Marshal(v)
	if string(out) != `{"cpu":"0.5","memory":"64Mi"}` {
		t.Fatalf("marshal: %s", out)
	}
	if err := json.Unmarshal([]byte(`{"cpu": "lots"}`), &v); err == nil {
		t.Fatal("invalid quantity accepted")
	}
}

func TestMetricsQuantities(t *testing.T) {
	for s, milli := range map[string]int64{"117586291n": 118, "1000000n": 1, "250u": 1, "0n": 0} {
		if q := MustParse(s); q.MilliValue() != milli {
			t.Errorf("%s: %d milli, want %d", s, q.MilliValue(), milli)
		}
	}
	if q := NewNanoQuantity(307831245); q.String() != "307831245n" || q.MilliValue() != 308 {
		t.Errorf("nano: %s %d", q, q.MilliValue())
	}
	for bytes, want := range map[int64]string{3059101696: "2987404Ki", 64 << 20: "64Mi", 1000: "1000", 0: "0", 3 << 30: "3Gi"} {
		if q := NewBinaryQuantity(bytes); q.String() != want || q.Value() != bytes {
			t.Errorf("%d bytes: %s (%d)", bytes, q, q.Value())
		}
	}
}
