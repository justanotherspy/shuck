package gateway

import "sync/atomic"

// Metrics counts gateway activity. All fields are safe for concurrent use.
// Counters are in-process only (reset on restart); cmd/shuck-gateway logs a
// periodic snapshot.
type Metrics struct {
	ConnectionsLive     atomic.Int64 // gauge: currently registered connections
	ConnectionsTotal    atomic.Int64 // connections accepted since start
	ConnectionsReplaced atomic.Int64 // closed by newest-wins replacement
	AuthRejected        atomic.Int64 // handshakes rejected (bad frame/token)
	HeartbeatFailures   atomic.Int64 // pings that timed out or errored

	EventsBuffered   atomic.Int64 // buffer rows written
	EventsPushed     atomic.Int64 // event frames written to live conns
	EventsAcked      atomic.Int64 // buffer rows deleted by ack
	EventsSuppressed atomic.Int64 // skipped as self-authored
	EventsDeduped    atomic.Int64 // deliver retries absorbed by event_id
	BufferDepth      atomic.Int64 // approximate gauge: +append, -ack

	ReplaySessions atomic.Int64 // connections that replayed at least one event
	ReplayEvents   atomic.Int64 // events sent during initial replay

	DeliverRequests atomic.Int64 // POST /internal/deliver calls accepted
	DeliverRejected atomic.Int64 // deliver calls rejected (401)
	// DeliverLatencySumMS / DeliverLatencyCount track deliver-receipt →
	// buffer-write latency per event; average is sum/count.
	DeliverLatencySumMS atomic.Int64
	DeliverLatencyCount atomic.Int64

	SweepRemoved atomic.Int64 // subscribers cleaned by the grace-window sweep
}
