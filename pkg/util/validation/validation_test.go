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
	if !IsEnvVarName("MY_VAR.1") || IsEnvVarName("1VAR") {
		t.Error("IsEnvVarName")
	}
}
