package intstr

import (
	"encoding/json"
	"testing"
)

func TestIntOrStringJSON(t *testing.T) {
	var v struct{ A, B IntOrString }
	if err := json.Unmarshal([]byte(`{"A": 8080, "B": "http"}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A.IsString || v.A.IntVal != 8080 || !v.B.IsString || v.B.StrVal != "http" {
		t.Fatalf("%+v", v)
	}
	out, _ := json.Marshal(v)
	if string(out) != `{"A":8080,"B":"http"}` {
		t.Fatalf("%s", out)
	}
}

func TestGetScaledValue(t *testing.T) {
	pct := func(s string) *IntOrString { v := FromString(s); return &v }
	num := func(n int) *IntOrString { v := FromInt(n); return &v }
	for _, tc := range []struct {
		v       *IntOrString
		total   int
		roundUp bool
		want    int
	}{
		{pct("25%"), 3, true, 1}, {pct("25%"), 3, false, 0}, {pct("25%"), 10, true, 3}, {pct("25%"), 10, false, 2},
		{pct("100%"), 4, false, 4}, {num(2), 10, true, 2}, {nil, 10, true, 0},
	} {
		if got, err := GetScaledValue(tc.v, tc.total, tc.roundUp); err != nil || got != tc.want {
			t.Errorf("%v of %d (up=%v): %d %v, want %d", tc.v, tc.total, tc.roundUp, got, err, tc.want)
		}
	}
	if _, err := GetScaledValue(pct("x%"), 10, true); err == nil {
		t.Error("invalid percentage accepted")
	}
	if _, err := GetScaledValue(pct("5"), 10, true); err == nil {
		t.Error("string without % accepted")
	}
}
