//go:build windows

package daemon

import (
	"errors"
	"syscall"
)

// Windows has no sessions or SIGTERM; background mode is not offered there.
const supported = false

func detached() *syscall.SysProcAttr { return nil }

func alive(int) bool { return false }

func terminate(int) error { return errors.ErrUnsupported }

func kill(int) error { return errors.ErrUnsupported }
