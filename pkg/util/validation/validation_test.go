package validation

import (
	"strings"
	"testing"
)

func TestNames(t *testing.T) {
	for s, want := range map[string]bool{"web": true, "web-1": true, "Web": false, "-web": false, "a.b": false, strings.Repeat("a", 64): false} {
		if IsDNS1123Label(s) != want {
			t.Errorf("IsDNS1123Label(%q) != %v", s, want)
		}
	}
	for s, want := range map[string]bool{"a.b-c": true, "a..b": false, strings.Repeat("a", 254): false} {
		if IsDNS1123Subdomain(s) != want {
			t.Errorf("IsDNS1123Subdomain(%q) != %v", s, want)
		}
	}
	// Kubernetes 1.34 relaxed environment variable names.
	for _, ok := range []string{"MY_VAR.1", "1VAR", "a-b", "has space", "~"} {
		if errs := IsRelaxedEnvVarName(ok); len(errs) != 0 {
			t.Errorf("IsRelaxedEnvVarName(%q): %v", ok, errs)
		}
	}
	for _, bad := range []string{"", "a=b", "tab\there", "café"} {
		if len(IsRelaxedEnvVarName(bad)) == 0 {
			t.Errorf("IsRelaxedEnvVarName(%q) accepted", bad)
		}
	}
	for key, want := range map[string]int{"key.name": 0, "KEY_NAME": 0, "a-b": 0, "a/b": 1, "has space": 1, ".": 1, "..": 1,
		"..x": 1, strings.Repeat("a", 254): 1, "": 1} {
		if got := IsConfigMapKey(key); len(got) != want {
			t.Errorf("IsConfigMapKey(%q) = %v", key, got)
		}
	}
}
