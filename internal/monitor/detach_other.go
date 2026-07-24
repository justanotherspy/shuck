//go:build windows || plan9 || js || wasip1

package monitor

import "syscall"

// detachAttr has nothing to set on platforms without POSIX sessions. The
// spawned daemon still outlives its parent there — it just shares the parent's
// console until that closes.
func detachAttr() *syscall.SysProcAttr { return nil }
