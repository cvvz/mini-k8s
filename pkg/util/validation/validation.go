// Package validation has generic name and identifier checks, mirroring
// k8s.io/apimachinery/pkg/util/validation.
package validation

import "regexp"

var (
	dns1123Label     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	envVarName       = regexp.MustCompile(`^[-._a-zA-Z][-._a-zA-Z0-9]*$`)
)

// IsDNS1123Label reports whether s is a lowercase RFC 1123 label (at most 63 characters).
func IsDNS1123Label(s string) bool { return dns1123Label.MatchString(s) }

// IsDNS1123Subdomain reports whether s is a lowercase RFC 1123 subdomain (at most 253 characters).
func IsDNS1123Subdomain(s string) bool { return len(s) <= 253 && dns1123Subdomain.MatchString(s) }

// IsEnvVarName reports whether s is a valid environment variable name.
func IsEnvVarName(s string) bool { return envVarName.MatchString(s) }
