// Package serverless is the API Gateway WebSocket variant of the gateway
// (JUS-92): the same wire protocol, stores, and deliver contract as the
// resident internal/gateway server, restructured for per-invocation Lambda
// handlers. API Gateway terminates the WebSockets; this package supplies the
// logic behind its $connect / $default / $disconnect routes and the deliver
// endpoint, pushing outbound frames through the @connections management API.
//
// Differences from the resident gateway, forced by the platform and
// mirrored by the channel shim's superset protocol:
//
//   - Application close codes cannot be sent through @connections, so the
//     4401/4409 verdicts become in-band control frames ("unauthorized",
//     "replaced") posted before the connection is dropped.
//   - There is no per-connection process, so replay happens on the hello
//     frame and pushes drain every unacked buffer row in seq order (acks
//     delete rows, so the buffer is exactly the unacked set).
//   - Liveness comes from the shim's application-level ping frames (which
//     refresh the durable presence row), not server heartbeats.
//
// The package is pure: DynamoDB and management-API adapters live in
// internal/gateway/awsx, and only cmd/shuck-gateway links them.
package serverless

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/justanotherspy/shuck/internal/gateway"
)

// Gateway → shim control frame types. The resident gateway expresses these
// as WebSocket close codes; API Gateway cannot, so they are frames.
const (
	// FrameUnauthorized tells the shim its token was rejected — the
	// serverless form of close 4401. The shim stops permanently.
	FrameUnauthorized = "unauthorized"
	// FrameReplaced tells an older connection a newer one took its
	// subscriber key — the serverless form of close 4409. The shim stops
	// permanently.
	FrameReplaced = "replaced"
)

// ErrGone reports that a connection no longer exists at the management API.
// ConnAPI implementations must translate their platform error to it.
var ErrGone = errors.New("serverless: connection gone")

// ConnAPI posts frames to (and drops) live connections — the @connections
// management API behind an interface so the core stays pure.
type ConnAPI interface {
	// Post writes one frame to a connection. A vanished connection is
	// reported as ErrGone (wrapped or bare).
	Post(ctx context.Context, connID string, data []byte) error
	// Close drops a connection. Closing an already-gone connection is not
	// an error.
	Close(ctx context.Context, connID string) error
}

// RegistryStore is the durable subscriber ↔ connection mapping — the
// serverless counterpart of the resident gateway's in-memory Registry. Rows
// live in the buffer table under discriminated sort keys and carry a TTL
// safely past API Gateway's two-hour connection cap, so crashes only ever
// leave garbage that expires.
type RegistryStore interface {
	// Set makes connID the current connection for sub and returns the
	// connection it displaced ("" if none) — newest wins, the caller
	// closes prev.
	Set(ctx context.Context, sub gateway.SubscriberKey, connID string) (prev string, err error)
	// Get returns sub's current connection, if any.
	Get(ctx context.Context, sub gateway.SubscriberKey) (connID string, ok bool, err error)
	// Lookup resolves a connection id back to its authenticated
	// subscriber. Unknown connections (never helloed, expired, replaced)
	// report ok=false.
	Lookup(ctx context.Context, connID string) (sub gateway.SubscriberKey, ok bool, err error)
	// Remove deletes connID's mapping and reports the subscriber it
	// belonged to. The forward mapping is only removed while it still
	// points at connID, so a replaced connection's disconnect never
	// disturbs its successor.
	Remove(ctx context.Context, connID string) (sub gateway.SubscriberKey, ok bool, err error)
}

// Gateway is the serverless gateway core. Route adapters call Connect /
// Message / Disconnect; the deliver endpoint calls Deliver (it satisfies
// gateway.EventDeliverer, so gateway.DeliverHandler fronts it unchanged).
type Gateway struct {
	Tokens   gateway.TokenStore
	Subs     gateway.SubscriptionStore
	Buffer   gateway.EventBuffer
	Presence gateway.PresenceStore
	Registry RegistryStore
	Conns    ConnAPI
	// Toucher may be nil, which disables last-used stamping on hello. It
	// runs synchronously (best-effort) — Lambda freezes background
	// goroutines, so fire-and-forget would silently never run.
	Toucher gateway.TokenToucher
	// Log may be nil, which means slog.Default().
	Log *slog.Logger
	// Metrics may be nil, which disables counting. Counters are
	// per-invocation in Lambda, so they surface in logs, not gauges.
	Metrics *gateway.Metrics
	// Now may be nil, which means time.Now.
	Now func() time.Time
}

// controlFrame is the wire form of the gateway → shim control frames.
type controlFrame struct {
	Type string `json:"type"`
}

func encodeControl(frameType string) []byte {
	data, _ := json.Marshal(controlFrame{Type: frameType}) // cannot fail
	return data
}

// Connect handles $connect. Authentication stays in the hello frame (the
// same wire protocol as the resident gateway), so a connect is always
// accepted; a connection that never authenticates cannot subscribe or
// receive anything and dies at API Gateway's idle timeout.
func (g *Gateway) Connect(_ context.Context, _ string) error {
	g.count(func(m *gateway.Metrics) { m.ConnectionsTotal.Add(1) })
	return nil
}

// Message handles one $default frame. Errors that are the client's fault
// close the connection; store failures also close it (the shim reconnects
// and retries), mirroring the resident gateway's verdicts. The returned
// error is always nil — API Gateway has no useful channel for it — but the
// signature leaves room for adapters that want one.
func (g *Gateway) Message(ctx context.Context, connID string, data []byte) error {
	frame, err := gateway.ParseClientFrame(data)
	if err != nil {
		g.log().Info("closing connection: bad frame", "conn", connID, "err", err)
		g.drop(ctx, connID)
		return nil
	}
	if frame.Type == gateway.FrameHello {
		// Hello must be the first frame and must not repeat — the resident
		// hub closes a repeat with StatusProtocolError. Re-running hello
		// here would re-register the connection and leave the previous
		// identity's forward row dangling (a Gone round-trip on every
		// deliver) until its TTL, so a repeat drops the connection; the
		// platform's $disconnect then cleans the existing registration.
		_, registered, err := g.Registry.Lookup(ctx, connID)
		if err != nil {
			g.log().Error("registry lookup failed", "conn", connID, "err", err)
			g.drop(ctx, connID)
			return nil
		}
		if registered {
			g.log().Info("closing connection: repeated hello", "conn", connID)
			g.drop(ctx, connID)
			return nil
		}
		g.hello(ctx, connID, frame)
		return nil
	}

	sub, ok, err := g.Registry.Lookup(ctx, connID)
	if err != nil {
		g.log().Error("registry lookup failed", "conn", connID, "err", err)
		g.drop(ctx, connID)
		return nil
	}
	if !ok {
		// Frames before a successful hello: protocol violation.
		g.log().Info("closing connection: frame before hello", "conn", connID, "type", frame.Type)
		g.drop(ctx, connID)
		return nil
	}

	switch frame.Type {
	case gateway.FramePing:
		// The shim's keepalive doubles as the durable liveness signal the
		// grace-window sweep reads.
		if err := g.Presence.Touch(ctx, sub, g.clock()()); err != nil {
			g.log().Warn("presence touch failed", "subscriber", sub.String(), "err", err)
		}
	case gateway.FrameSubscribe, gateway.FrameUnsubscribe:
		ref := gateway.PRRef{Repo: frame.Repo, PR: frame.PR}
		op := g.Subs.Subscribe
		if frame.Type == gateway.FrameUnsubscribe {
			op = g.Subs.Unsubscribe
		}
		if err := op(ctx, ref, sub); err != nil {
			// The shim believes the (un)subscribe took effect; a silent
			// failure would wedge it, so force a reconnect.
			g.log().Error("subscription write failed", "subscriber", sub.String(), "pr", ref.String(), "err", err)
			g.drop(ctx, connID)
			return nil
		}
		g.log().Info(frame.Type, "subscriber", sub.String(), "pr", ref.String())
	case gateway.FrameAck:
		if err := g.Buffer.Ack(ctx, sub, frame.ID); err != nil {
			// Non-fatal: the row lingers until its TTL.
			g.log().Warn("ack failed", "subscriber", sub.String(), "id", frame.ID, "err", err)
			return nil
		}
		g.count(func(m *gateway.Metrics) { m.EventsAcked.Add(1) })
	}
	return nil
}

// hello authenticates the connection, applies newest-wins replacement, and
// replays every unacked buffered event. A rejected token gets the
// "unauthorized" control frame (the shim stops permanently); a store
// failure just drops the connection (the shim retries).
func (g *Gateway) hello(ctx context.Context, connID string, frame gateway.ClientFrame) {
	hash := gateway.HashToken(frame.Token)
	rec, err := g.Tokens.Lookup(ctx, hash)
	switch {
	case errors.Is(err, gateway.ErrTokenNotFound):
		g.count(func(m *gateway.Metrics) { m.AuthRejected.Add(1) })
		g.log().Info("connection rejected", "conn", connID, "reason", "unknown token")
		if err := g.Conns.Post(ctx, connID, encodeControl(FrameUnauthorized)); err != nil && !errors.Is(err, ErrGone) {
			g.log().Warn("unauthorized frame post failed", "conn", connID, "err", err)
		}
		g.drop(ctx, connID)
		return
	case err != nil:
		// A store failure is not an auth verdict: drop without a control
		// frame so the shim retries instead of discarding its token.
		g.log().Error("token lookup failed", "err", err)
		g.drop(ctx, connID)
		return
	}
	if g.Toucher != nil {
		if err := g.Toucher.TouchToken(ctx, hash, g.clock()()); err != nil {
			g.log().Warn("token touch failed", "err", err)
		}
	}

	sub := gateway.SubscriberKey{
		UserID:    strconv.FormatInt(rec.GitHubUserID, 10),
		SessionID: frame.SessionID,
	}
	prev, err := g.Registry.Set(ctx, sub, connID)
	if err != nil {
		g.log().Error("registry set failed", "subscriber", sub.String(), "err", err)
		g.drop(ctx, connID)
		return
	}
	if prev != "" && prev != connID {
		g.count(func(m *gateway.Metrics) { m.ConnectionsReplaced.Add(1) })
		if err := g.Conns.Post(ctx, prev, encodeControl(FrameReplaced)); err != nil && !errors.Is(err, ErrGone) {
			g.log().Warn("replaced frame post failed", "conn", prev, "err", err)
		}
		if err := g.Conns.Close(ctx, prev); err != nil {
			g.log().Warn("replaced connection close failed", "conn", prev, "err", err)
		}
	}
	if err := g.Presence.Touch(ctx, sub, g.clock()()); err != nil {
		g.log().Warn("presence touch failed", "subscriber", sub.String(), "err", err)
	}
	g.log().Info("connection registered", "subscriber", sub.String(), "conn", connID)

	// Replay everything unacked. Acks delete buffer rows, so a full replay
	// is exactly the outstanding set; the shim dedupes by event id, so
	// re-sending an in-flight unacked event is harmless. (The resident
	// gateway's last_event_id cursor is an optimization a per-invocation
	// handler doesn't need — and ignoring it can never lose an event,
	// where an out-of-order cursor could.)
	n, err := g.push(ctx, sub, connID)
	if err != nil {
		g.log().Error("replay failed", "subscriber", sub.String(), "err", err)
		return
	}
	if n > 0 {
		g.count(func(m *gateway.Metrics) {
			m.ReplaySessions.Add(1)
			m.ReplayEvents.Add(int64(n))
		})
	}
}

// Disconnect handles $disconnect: remove the connection mapping and mark
// the subscriber disconnected for the grace-window sweep. Connections that
// never authenticated have no mapping and nothing to do.
func (g *Gateway) Disconnect(ctx context.Context, connID string) {
	sub, ok, err := g.Registry.Remove(ctx, connID)
	if err != nil {
		g.log().Warn("registry remove failed", "conn", connID, "err", err)
		return
	}
	if !ok {
		return
	}
	if err := g.Presence.MarkDisconnected(ctx, sub, g.clock()()); err != nil {
		g.log().Warn("mark disconnected failed", "subscriber", sub.String(), "err", err)
	}
	g.log().Info("connection closed", "subscriber", sub.String(), "conn", connID)
}

// Deliver fans one worker event out to the PR's subscribers with the same
// write-then-push semantics and DeliverResult accounting as the resident
// Hub: suppression check, buffer append (transactional with the event_id
// dedupe marker), then a drain-push to the live connection if any. A
// pr_closed event additionally removes every subscription for the PR —
// after buffering, so offline subscribers still replay the final event.
// Per-subscriber failures don't stop the fan-out; the joined error tells
// the worker to retry (the dedupe marker absorbs the overlap).
func (g *Gateway) Deliver(ctx context.Context, req gateway.DeliverRequest) (gateway.DeliverResult, error) {
	ref := gateway.PRRef{Repo: req.Repo, PR: req.PR}
	subs, err := g.Subs.Subscribers(ctx, ref)
	if err != nil {
		return gateway.DeliverResult{}, err
	}
	res := gateway.DeliverResult{Subscribers: len(subs)}
	var errs []error
	ev := req.Event()
	for _, sub := range subs {
		if req.Suppressed(sub.UserID) {
			res.Suppressed++
			g.count(func(m *gateway.Metrics) { m.EventsSuppressed.Add(1) })
			continue
		}
		start := g.clock()()
		_, dup, err := g.Buffer.Append(ctx, sub, ev)
		if err != nil {
			g.log().Error("buffer append failed", "subscriber", sub.String(), "id", req.EventID, "err", err)
			errs = append(errs, err)
			continue
		}
		elapsed := g.clock()().Sub(start).Milliseconds()
		g.count(func(m *gateway.Metrics) {
			m.DeliverLatencySumMS.Add(elapsed)
			m.DeliverLatencyCount.Add(1)
		})
		if dup {
			res.Deduped++
			g.count(func(m *gateway.Metrics) { m.EventsDeduped.Add(1) })
			continue
		}
		res.Buffered++
		g.count(func(m *gateway.Metrics) { m.EventsBuffered.Add(1) })

		connID, ok, err := g.Registry.Get(ctx, sub)
		if err != nil {
			// The event is safely buffered; the next reconnect replays it.
			g.log().Warn("registry get failed", "subscriber", sub.String(), "err", err)
			continue
		}
		if !ok {
			continue
		}
		// Drain-push everything unacked (not just this event): if an
		// earlier concurrent deliver lost the race between its append and
		// its push, this flush carries its event too, in seq order.
		if _, err := g.push(ctx, sub, connID); err != nil {
			g.log().Warn("push failed", "subscriber", sub.String(), "err", err)
			continue
		}
		res.Pushed++
	}
	if req.Kind == gateway.KindPRClosed {
		if err := g.Subs.RemoveAllForPR(ctx, ref); err != nil {
			g.log().Error("pr_closed cleanup failed", "pr", ref.String(), "err", err)
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

// push sends every buffered event for sub to connID in seq order and
// returns how many frames were posted. A Gone connection triggers the same
// cleanup as $disconnect; buffered rows are untouched either way (only acks
// delete them), so any failure is recovered by the next reconnect's replay.
func (g *Gateway) push(ctx context.Context, sub gateway.SubscriberKey, connID string) (int, error) {
	events, err := g.Buffer.After(ctx, sub, 0)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, ev := range events {
		data, err := ev.Encode()
		if err != nil {
			g.log().Error("encode event failed", "id", ev.ID, "err", err)
			continue
		}
		if err := g.Conns.Post(ctx, connID, data); err != nil {
			if errors.Is(err, ErrGone) {
				g.log().Info("connection gone during push", "subscriber", sub.String(), "conn", connID)
				g.Disconnect(ctx, connID)
				return sent, nil
			}
			return sent, err
		}
		sent++
		g.count(func(m *gateway.Metrics) { m.EventsPushed.Add(1) })
	}
	return sent, nil
}

// drop closes a connection best-effort.
func (g *Gateway) drop(ctx context.Context, connID string) {
	if err := g.Conns.Close(ctx, connID); err != nil {
		g.log().Warn("connection close failed", "conn", connID, "err", err)
	}
}

func (g *Gateway) log() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.Default()
}

func (g *Gateway) clock() func() time.Time {
	if g.Now != nil {
		return g.Now
	}
	return time.Now
}

func (g *Gateway) count(f func(*gateway.Metrics)) {
	if g.Metrics != nil {
		f(g.Metrics)
	}
}
