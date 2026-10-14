package etcdtesting

import (
	"os/exec"
	"syscall"
)

// bindLifetime kills etcd when the test process exits.
func bindLifetime(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
