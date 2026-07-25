//go:build !windows && !plan9 && !js && !wasip1

package monitor

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether pid still names a running process. Signal 0 is
// the POSIX existence probe: the kernel checks the pid and the permission and
// delivers nothing. EPERM is a live process owned by somebody else — a stream
// started by another user is still a stream — so only ESRCH, or a pid that
// could never be one, counts as gone.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
