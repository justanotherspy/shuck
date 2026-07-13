package gateway

import (
	"sync/atomic"

	"github.com/justanotherspy/shuck/internal/promexpo"
)

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

// Snapshot renders the current counters as Prometheus samples for the opt-in
// /metrics endpoint. Names are the same fields cmd/shuck-gateway logs,
// namespaced under shuck_gateway_ and suffixed _total for counters. A nil
// receiver returns nil, so callers need not guard.
func (m *Metrics) Snapshot() []promexpo.Sample {
	if m == nil {
		return nil
	}
	c := func(name, help string, v int64) promexpo.Sample {
		return promexpo.Sample{Name: name, Help: help, Type: promexpo.Counter, Value: v}
	}
	g := func(name, help string, v int64) promexpo.Sample {
		return promexpo.Sample{Name: name, Help: help, Type: promexpo.Gauge, Value: v}
	}
	return []promexpo.Sample{
		g("shuck_gateway_connections_live", "Currently registered shim connections.", m.ConnectionsLive.Load()),
		c("shuck_gateway_connections_total", "Connections accepted since start.", m.ConnectionsTotal.Load()),
		c("shuck_gateway_connections_replaced_total", "Connections closed by newest-wins replacement.", m.ConnectionsReplaced.Load()),
		c("shuck_gateway_auth_rejected_total", "Handshakes rejected (bad frame or token).", m.AuthRejected.Load()),
		c("shuck_gateway_heartbeat_failures_total", "Heartbeat pings that timed out or errored.", m.HeartbeatFailures.Load()),
		c("shuck_gateway_events_buffered_total", "Event buffer rows written.", m.EventsBuffered.Load()),
		c("shuck_gateway_events_pushed_total", "Event frames written to live connections.", m.EventsPushed.Load()),
		c("shuck_gateway_events_acked_total", "Buffer rows deleted by ack.", m.EventsAcked.Load()),
		c("shuck_gateway_events_suppressed_total", "Events skipped as self-authored.", m.EventsSuppressed.Load()),
		c("shuck_gateway_events_deduped_total", "Deliver retries absorbed by event_id.", m.EventsDeduped.Load()),
		g("shuck_gateway_buffer_depth", "Approximate unacked buffer depth (+append, -ack).", m.BufferDepth.Load()),
		c("shuck_gateway_replay_sessions_total", "Connections that replayed at least one event.", m.ReplaySessions.Load()),
		c("shuck_gateway_replay_events_total", "Events sent during initial replay.", m.ReplayEvents.Load()),
		c("shuck_gateway_deliver_requests_total", "POST /internal/deliver calls accepted.", m.DeliverRequests.Load()),
		c("shuck_gateway_deliver_rejected_total", "Deliver calls rejected (401).", m.DeliverRejected.Load()),
		c("shuck_gateway_deliver_latency_ms_sum", "Sum of deliver-receipt to buffer-write latency, milliseconds.", m.DeliverLatencySumMS.Load()),
		c("shuck_gateway_deliver_latency_ms_count", "Count of deliver latency observations.", m.DeliverLatencyCount.Load()),
		c("shuck_gateway_sweep_removed_total", "Subscribers cleaned by the grace-window sweep.", m.SweepRemoved.Load()),
	}
}
