//go:build !windows

package daemon

import (
	"errors"
	"syscall"
)

const supported = true

// detached starts the process in its own session, so it has no controlling
// terminal and is not hit by the terminal's hangup or Ctrl-C.
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func kill(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
