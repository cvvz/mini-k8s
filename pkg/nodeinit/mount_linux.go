package nodeinit

import "syscall"

// makeMountsShared lets mounts made by containerd propagate, like
// `mount --make-rshared /`.
func makeMountsShared() error {
	return syscall.Mount("none", "/", "", syscall.MS_REC|syscall.MS_SHARED, "")
}
