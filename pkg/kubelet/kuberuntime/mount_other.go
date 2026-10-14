//go:build !linux

package kuberuntime

import "errors"

// SystemMounter is only available on Linux, where kubelets run.
type SystemMounter struct{}

// MountTmpfs fails outside Linux.
func (SystemMounter) MountTmpfs(string, int64) error {
	return errors.New("tmpfs volumes require Linux")
}

// Unmount fails outside Linux.
func (SystemMounter) Unmount(string) error { return errors.New("tmpfs volumes require Linux") }
