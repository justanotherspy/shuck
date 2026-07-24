//go:build !windows && !plan9 && !js && !wasip1

package monitor

import "syscall"

// detachAttr puts the spawned daemon in its own session. Without it the daemon
// stays in the terminal's process group and a Ctrl-C aimed at the command that
// started it — or the shell exiting — takes the monitor down with it, which is
// exactly the opposite of what a background monitor is for.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
