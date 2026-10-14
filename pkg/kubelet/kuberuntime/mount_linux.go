package kuberuntime

import (
	"fmt"
	"syscall"
)

// SystemMounter mounts with mount(2); the kubelet runs in the node
// container's mount namespace, which containerd shares.
type SystemMounter struct{}

// MountTmpfs mounts a tmpfs of sizeBytes (0: the kernel default) on dir.
func (SystemMounter) MountTmpfs(dir string, sizeBytes int64) error {
	opts := ""
	if sizeBytes > 0 {
		opts = fmt.Sprintf("size=%d", sizeBytes)
	}
	return syscall.Mount("tmpfs", dir, "tmpfs", 0, opts)
}

// Unmount unmounts dir.
func (SystemMounter) Unmount(dir string) error { return syscall.Unmount(dir, 0) }
