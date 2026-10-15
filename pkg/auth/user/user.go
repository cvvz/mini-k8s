// Package user describes authenticated identities, like
// k8s.io/apiserver/pkg/authentication/user.
package user

// Well-known users and groups.
const (
	SystemPrivilegedGroup = "system:masters"
	AllAuthenticated      = "system:authenticated"
	AllUnauthenticated    = "system:unauthenticated"
	Anonymous             = "system:anonymous"
	// AdminName is the user of the cluster token (kubeadm's admin.conf
	// user), a member of system:masters.
	AdminName = "kubernetes-admin"
)

// Info is who made a request.
type Info struct {
	Name   string
	UID    string
	Groups []string
}

// InGroup reports whether the user belongs to group.
func (u *Info) InGroup(group string) bool {
	for _, g := range u.Groups {
		if g == group {
			return true
		}
	}
	return false
}
