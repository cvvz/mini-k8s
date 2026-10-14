//go:build !linux

package nodeinit

import "errors"

func makeMountsShared() error { return errors.New("node-init requires Linux") }
