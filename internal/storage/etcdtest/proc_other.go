//go:build !linux

package etcdtest

import "os/exec"

// bindLifetime is a no-op where Pdeathsig is unavailable.
func bindLifetime(*exec.Cmd) {}
