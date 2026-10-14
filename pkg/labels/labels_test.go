package labels

import "testing"

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
		sel, err := Parse(tc.sel)
		if err != nil {
			t.Fatalf("%q: %v", tc.sel, err)
		}
		if got := sel.Matches(labels); got != tc.want {
			t.Errorf("%q matched=%v want %v", tc.sel, got, tc.want)
		}
	}
	for _, bad := range []string{"app in prod", "app in ()", "a b c", "app=we b", "-bad=x"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q: expected parse error", bad)
		}
	}
}
