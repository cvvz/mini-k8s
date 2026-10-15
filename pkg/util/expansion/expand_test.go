package expansion

import "testing"

func TestExpand(t *testing.T) {
	mapping := MappingFuncFor(map[string]string{"A": "1", "EMPTY": ""}, map[string]string{"A": "shadowed", "SVC": "10.96.0.1"})
	for in, want := range map[string]string{
		"a is $(A), b is $(B), esc $$(A)": "a is 1, b is $(B), esc $(A)",
		"$(SVC):$(A)$(A)":                 "10.96.0.1:11",
		"[$(EMPTY)]":                      "[]",
		"$":                               "$",
		"cost $5 and $$":                  "cost $5 and $",
		"$(unterminated":                  "$(unterminated",
		"$()":                             "$()",
		"plain":                           "plain",
	} {
		if got := Expand(in, mapping); got != want {
			t.Errorf("Expand(%q) = %q, want %q", in, got, want)
		}
	}
}
