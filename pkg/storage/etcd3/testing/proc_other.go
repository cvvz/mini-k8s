//go:build !linux

package etcdtesting

import "os/exec"

// bindLifetime is a no-op where Pdeathsig is unavailable.
func bindLifetime(*exec.Cmd) {}
