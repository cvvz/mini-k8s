// Package network holds the node networking pieces: the mini-cni plugin and
// the host-gw route agent. They drive iproute2 and iptables through a Runner
// so their logic can be tested without root.
package network

import (
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes a command and returns its combined output.
type Runner func(name string, args ...string) (string, error)

// Exec runs real commands.
func Exec(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
