// Package validation has generic name and identifier checks, mirroring
// k8s.io/apimachinery/pkg/util/validation.
package validation

import (
	"regexp"
	"strings"
)

var (
	dns1123Label     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	configMapKey     = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
)

// Messages of the checks below, worded as in Kubernetes.
const (
	DNS1123SubdomainMessage = "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', " +
		"and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is " +
		`'[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')`
	ConfigMapKeyMessage = "a valid config key must consist of alphanumeric characters, '-', '_' or '.' " +
		"(e.g. 'key.name',  or 'KEY_NAME',  or 'key-name', regex used for validation is '[-._a-zA-Z0-9]+')"
	RelaxedEnvVarNameMessage = "a valid environment variable name must consist only of printable ASCII characters other than '='"
)

// IsDNS1123Label reports whether s is a lowercase RFC 1123 label (at most 63 characters).
func IsDNS1123Label(s string) bool { return dns1123Label.MatchString(s) }

// IsDNS1123Subdomain reports whether s is a lowercase RFC 1123 subdomain (at most 253 characters).
func IsDNS1123Subdomain(s string) bool { return len(s) <= 253 && dns1123Subdomain.MatchString(s) }

// IsRelaxedEnvVarName returns the problems with an environment variable
// name: since Kubernetes 1.34 any printable ASCII character but '=' is
// allowed.
func IsRelaxedEnvVarName(s string) []string {
	if s == "" {
		return []string{"environment variable name must be non-empty"}
	}
	for _, r := range s {
		if r < ' ' || r > '~' || r == '=' {
			return []string{RelaxedEnvVarNameMessage}
		}
	}
	return nil
}

// IsConfigMapKey returns the problems with a ConfigMap or Secret key: at
// most 253 characters of [-._a-zA-Z0-9], and not "." or "..".
func IsConfigMapKey(s string) []string {
	var errs []string
	if len(s) > 253 {
		errs = append(errs, "must be no more than 253 characters")
	}
	if !configMapKey.MatchString(s) {
		errs = append(errs, ConfigMapKeyMessage)
	}
	switch {
	case s == ".":
		errs = append(errs, "must not be '.'")
	case s == "..":
		errs = append(errs, "must not be '..'")
	case strings.HasPrefix(s, ".."):
		errs = append(errs, "must not start with '..'")
	}
	return errs
}
