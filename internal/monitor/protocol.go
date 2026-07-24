package monitor

import (
	"encoding/json"
	"fmt"
	"time"
)

// The local protocol between the daemon and its short-lived clients: one JSON
// request per line, one JSON response per line, connection closed after the
// exchange. It is deliberately dull. Everything on both ends of it ships in the
// same binary and is upgraded together, so there is no version negotiation to
// get wrong — a client that finds a daemon it cannot talk to restarts it.

// Op names a request.
type Op string

const (
	// OpPing checks the daemon is alive and reports its version.
	OpPing Op = "ping"
	// OpStatus returns the whole picture: watches, targets, poll state.
	OpStatus Op = "status"
	// OpWatch registers something to follow.
	OpWatch Op = "watch"
	// OpUnwatch drops a watch.
	OpUnwatch Op = "unwatch"
	// OpEvents drains a consumer's pending events, optionally waiting for the
	// next one to arrive.
	OpEvents Op = "events"
	// OpSeek moves a consumer's cursor to the present without delivering.
	OpSeek Op = "seek"
	// OpPoke asks for an immediate re-check, for the moment right after a push
	// when waiting out the poll interval is pure latency.
	OpPoke Op = "poke"
	// OpStop shuts the daemon down.
	OpStop Op = "stop"
)

// Request is one client call.
type Request struct {
	Op Op `json:"op"`
	// Auth carries the bearer token a loopback endpoint requires. A unix
	// socket needs none and ignores it.
	Auth string `json:"auth,omitempty"`
	// Watch describes what to follow, for OpWatch.
	Watch *Watch `json:"watch,omitempty"`
	// ID names an existing watch, for OpUnwatch and OpPoke ("" pokes
	// everything).
	ID string `json:"id,omitempty"`
	// Consumer is the stable identity whose cursor OpEvents and OpSeek move —
	// a Claude Code session ID, typically. Empty means "peek without
	// consuming".
	Consumer string `json:"consumer,omitempty"`
	// Limit caps how many events OpEvents returns (0 = no cap).
	Limit int `json:"limit,omitempty"`
	// Wait blocks OpEvents for up to this long when nothing is pending, so an
	// agent can wait for CI to finish without polling the daemon.
	Wait time.Duration `json:"wait,omitempty"`
	// Since, when non-zero, overrides the consumer cursor for OpEvents and
	// gives the cursor position for OpSeek.
	Since uint64 `json:"since,omitempty"`
	// All asks OpEvents for the whole retained journal rather than what is
	// pending.
	All bool `json:"all,omitempty"`
	// Peek returns the consumer's pending events without advancing its
	// cursor. The Stop hook needs it: it has to look at what is waiting to
	// decide whether to act on it, and events it decides not to act on must
	// still be there for the next prompt.
	Peek bool `json:"peek,omitempty"`
}

// Response is the daemon's reply. Exactly one of Error and the payload fields
// is meaningful.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`

	Version string   `json:"version,omitempty"`
	Status  *Status  `json:"status,omitempty"`
	Watch   *Watch   `json:"watch,omitempty"`
	Events  []Event  `json:"events,omitempty"`
	Cursor  uint64   `json:"cursor,omitempty"`
	Message string   `json:"message,omitempty"`
	Dropped []string `json:"dropped,omitempty"`
}

// Status is the daemon's whole state, as `shuck monitor status` reports it.
type Status struct {
	// PID and Version identify the running daemon.
	PID     int    `json:"pid"`
	Version string `json:"version"`
	// StartedAt is when the daemon came up; Uptime is derived for readers who
	// would otherwise have to subtract.
	StartedAt time.Time     `json:"started_at"`
	Uptime    time.Duration `json:"uptime"`
	// Watches is what the monitor has been told to follow.
	Watches []Watch `json:"watches"`
	// Targets is what those watches currently resolve to, deduplicated —
	// the things actually being polled.
	Targets []TargetStatus `json:"targets"`
	// Events is the journal's high-water mark, and Pending is how many of them
	// the asking consumer has not seen.
	Events  uint64 `json:"events"`
	Pending int    `json:"pending,omitempty"`
	// RateRemaining and RateLimit report the GitHub token's REST quota, so a
	// monitor that has gone quiet can be told apart from one that has run out
	// of requests. Both are 0 when the quota could not be read.
	RateRemaining int `json:"rate_remaining,omitempty"`
	RateLimit     int `json:"rate_limit,omitempty"`
}

// TargetStatus is one polled pull request's state.
type TargetStatus struct {
	Target string `json:"target"`
	// Verdict is the last CI verdict for the current head commit: "passed",
	// "failed", or "" while checks are still running or none have registered.
	Verdict string `json:"verdict,omitempty"`
	// HeadSHA is the commit the checks belong to.
	HeadSHA string `json:"head_sha,omitempty"`
	// Lifecycle is open / draft / merged / closed.
	Lifecycle string `json:"lifecycle,omitempty"`
	// LastPolled and NextPoll bracket the poll cadence.
	LastPolled time.Time `json:"last_polled,omitzero"`
	NextPoll   time.Time `json:"next_poll,omitzero"`
	// LastError is the most recent poll failure, when the target is failing.
	LastError string `json:"last_error,omitempty"`
	// Watches lists the watch IDs that resolve to this target.
	Watches []string `json:"watches,omitempty"`
}

// errResponse builds a failed reply.
func errResponse(err error) Response {
	return Response{OK: false, Error: err.Error()}
}

// endpoint is how a client finds the daemon: the network and address to dial,
// plus a bearer token when the transport is one any local process could reach.
// It is written by the daemon after it is listening and removed when it exits,
// so its presence is a hint, never a promise.
type endpoint struct {
	Network string `json:"network"`
	Address string `json:"address"`
	// Token authenticates a TCP client. A unix socket needs none: the
	// directory's permissions already restrict who can connect.
	Token string `json:"token,omitempty"`
	PID   int    `json:"pid"`
}

// decodeLine parses one protocol line into v, with a message that says which
// side sent the garbage.
func decodeLine(line []byte, v any) error {
	if err := json.Unmarshal(line, v); err != nil {
		return fmt.Errorf("malformed monitor message: %w", err)
	}
	return nil
}
