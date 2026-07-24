// Package monitor is shuck's local background monitor: a long-lived process
// that keeps track of the pull requests you are actually working on and turns
// what changes on GitHub into a stream of events an agent can consume.
//
// It replaces the "run a command and wait" loop with a subscription. You point
// it at a working tree once — or let a Claude Code session point it there for
// you — and from then on it follows that tree: it reads the branch out of
// HEAD, finds the open PR for it, and re-checks on a cadence that tightens
// while CI is running and relaxes when nothing is happening. Switch branches,
// change repositories, or open a second worktree and the monitor retargets
// itself; nothing has to be told a PR number.
//
// Everything is local. There is no webhook, no server to deploy, and no
// credential beyond the GitHub token the CLI already uses. The daemon owns one
// lock file, one socket, and one append-only journal under ~/.cache/shuck/monitor;
// short-lived clients (the `shuck monitor` subcommands, the MCP tools, and the
// Claude Code hooks) connect, ask a question, and exit.
//
// The package is layered so the interesting parts are testable without a
// network or a running daemon:
//
//   - git.go     reads a working tree's repository and branch, worktrees included
//   - event.go   the event model and its agent-facing rendering
//   - journal.go the durable event log and per-consumer cursors
//   - watch.go   the set of things being followed, and their poll state
//   - poll.go    one watch's GitHub round: what changed, and which events that means
//   - protocol.go / server.go / client.go  the local IPC between daemon and CLI
//   - hook.go    the Claude Code hook entry points
package monitor

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/justanotherspy/shuck/internal/cache"
)

// dirPerm/filePerm guard the monitor's state. The journal holds CI logs and
// review comments from private repositories, so — like the inspection cache it
// sits beside — it stays readable only by its owner.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// Poll cadence. The monitor spends most of its life waiting, so the intervals
// are chosen to make the common case cheap and the interesting case fast:
// while a check is running the answer changes every few seconds and is worth
// asking for; once everything is terminal only a push or a reviewer changes
// anything, and those are rare.
const (
	// ActiveInterval paces polling while a run is still in flight.
	ActiveInterval = 12 * time.Second
	// IdleInterval paces polling for an open PR whose checks are all terminal.
	IdleInterval = 90 * time.Second
	// DormantInterval paces polling for a watch with nothing to watch — a
	// branch with no open PR, or a merged one.
	DormantInterval = 5 * time.Minute
	// MaxBackoff caps the exponential backoff applied after poll errors.
	MaxBackoff = 15 * time.Minute
	// LowRateThreshold is the remaining REST quota below which the monitor
	// doubles its intervals rather than risk exhausting a shared token.
	LowRateThreshold = 500
)

// ResolveInterval is how often a tree watch that has not found a pull request
// re-asks. Finding one is a real API call, and a branch acquires a PR at the
// speed of someone opening one, so asking every tick would burn quota to learn
// nothing. A checkout that moves is re-resolved immediately regardless.
const ResolveInterval = time.Minute

// DefaultWatchTTL is how long a watch survives without any client asking about
// it. A laptop left closed overnight should not still be polling GitHub in the
// morning: sessions come and go, and a watch nobody has looked at in half a day
// is almost certainly the residue of one that ended. Every client request
// refreshes the watches it touches.
const DefaultWatchTTL = 12 * time.Hour

// Dir returns the monitor's state directory (~/.cache/shuck/monitor), creating
// it if needed. It sits under the same SHUCK_HOME-overridable base as the
// inspection cache, so a test can redirect the whole of shuck's on-disk state
// with one environment variable.
func Dir() (string, error) {
	base, err := cache.Base()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "monitor")
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("create monitor directory: %w", err)
	}
	return dir, nil
}

// paths names the files the daemon owns inside Dir.
type paths struct {
	dir string
	// endpoint is how clients dial: the network and address, plus a token when
	// the transport is one any local process could reach. There is no separate
	// lock file — binding the socket is the lock.
	endpoint string
	journal  string // append-only event log
	cursors  string // per-consumer delivery cursors
	watches  string // the persisted watch set
	log      string // the daemon's own diagnostic log
	socket   string // the unix socket, when one can be used
}

func newPaths(dir string) paths {
	return paths{
		dir:      dir,
		endpoint: filepath.Join(dir, "endpoint.json"),
		journal:  filepath.Join(dir, "events.jsonl"),
		cursors:  filepath.Join(dir, "cursors.json"),
		watches:  filepath.Join(dir, "watches.json"),
		log:      filepath.Join(dir, "daemon.log"),
		socket:   filepath.Join(dir, "daemon.sock"),
	}
}

// writeFileAtomic writes data to name via a temporary file in the same
// directory and a rename, so a reader either sees the previous contents or the
// new ones and never a half-written file. The daemon rewrites its state on
// every tick while clients read it at will, which makes the atomicity load
// bearing rather than decorative.
func writeFileAtomic(name string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), filepath.Base(name)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", name, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best effort: on the success path the rename has already consumed
		// the temp file and this removal is a no-op.
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(filePerm); err != nil {
		tmp.Close()
		return fmt.Errorf("set permissions on %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, name); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpName, name, err)
	}
	return nil
}
