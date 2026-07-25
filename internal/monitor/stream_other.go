//go:build windows || plan9 || js || wasip1

package monitor

// processAlive has no portable question to ask on these platforms — signal 0 is
// a POSIX idea, and os.FindProcess succeeds regardless — so it answers yes and
// leaves the heartbeat as the only test of liveness. That is the safe direction:
// a crashed stream's marker then goes stale on its own within streamStaleAfter,
// whereas answering no would mean no marker ever counted as live and every
// session got its CI failures twice.
func processAlive(int) bool { return true }
