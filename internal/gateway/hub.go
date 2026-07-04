package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Default tuning for Hub's optional knobs.
const (
	// DefaultHandshakeTimeout bounds the wait for the hello frame.
	DefaultHandshakeTimeout = 10 * time.Second
	// DefaultHeartbeat is the ping interval; quiet subscriptions must
	// outlive LB idle timeouts (nginx defaults to 60s), so pings are
	// frequent relative to those.
	DefaultHeartbeat = 30 * time.Second
	// DefaultPingTimeout bounds the wait for a pong before the peer is
	// declared dead.
	DefaultPingTimeout = 10 * time.Second
	// DefaultTouchInterval is how often a live connection refreshes its
	// durable presence row. The grace window is hours, so coarse is fine.
	DefaultTouchInterval = time.Hour
)

// Hub wires the registry and stores into the WebSocket endpoint and the
// deliver fan-out. Fields mirror ingest.Handler: required stores, nil-able
// optionals with lazy defaults.
type Hub struct {
	Tokens   TokenStore
	Subs     SubscriptionStore
	Buffer   EventBuffer
	Presence PresenceStore
	// Toucher may be nil, which disables last-used stamping on hello.
	Toucher TokenToucher
	// Registry may be nil, which means a process-local NewMemRegistry.
	Registry Registry
	// Log may be nil, which means slog.Default().
	Log *slog.Logger
	// Metrics may be nil, which disables counting.
	Metrics *Metrics
	// HandshakeTimeout, Heartbeat, PingTimeout, and TouchInterval fall
	// back to their Default* constants when zero.
	HandshakeTimeout time.Duration
	Heartbeat        time.Duration
	PingTimeout      time.Duration
	TouchInterval    time.Duration
	// Now may be nil, which means time.Now.
	Now func() time.Time

	draining     atomic.Bool
	conns        sync.WaitGroup
	registryOnce sync.Once
	registry     Registry
}

// DeliverResult reports what one deliver call did, per subscriber.
type DeliverResult struct {
	Subscribers int `json:"subscribers"`
	Buffered    int `json:"buffered"`
	Pushed      int `json:"pushed"`
	Suppressed  int `json:"suppressed"`
	Deduped     int `json:"deduped"`
}

// Reg returns the live-connection registry, defaulting to the in-memory
// implementation.
func (h *Hub) Reg() Registry {
	h.registryOnce.Do(func() {
		if h.Registry != nil {
			h.registry = h.Registry
			return
		}
		h.registry = NewMemRegistry()
	})
	return h.registry
}

// Draining reports whether Drain has started; readiness endpoints flip on
// it.
func (h *Hub) Draining() bool {
	return h.draining.Load()
}

// ServeWS is the GET /ws endpoint: upgrade, hello handshake, then serve the
// connection until it closes. It blocks for the connection's lifetime.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	if h.Draining() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		h.log().Debug("websocket accept failed", "err", err)
		return
	}
	ws.SetReadLimit(MaxFrameSize)

	key, cursor, ok := h.handshake(r.Context(), ws)
	if !ok {
		return
	}

	conn := newConn(r.Context(), key, ws)
	h.conns.Add(1)
	defer h.conns.Done()
	if prev := h.Reg().Register(key, conn); prev != nil {
		prev.shutdown(websocket.StatusCode(CloseReplaced), "replaced by newer connection")
		h.count(func(m *Metrics) { m.ConnectionsReplaced.Add(1) })
	}
	h.count(func(m *Metrics) { m.ConnectionsTotal.Add(1); m.ConnectionsLive.Add(1) })
	if h.Draining() {
		// Drain may have snapshotted between the entry check and the
		// registration above; close rather than hold the drain open.
		conn.shutdown(websocket.StatusGoingAway, "gateway draining")
	}
	if err := h.Presence.Touch(conn.ctx, key, h.clock()()); err != nil {
		h.log().Warn("presence touch failed", "subscriber", key.String(), "err", err)
	}
	h.log().Info("connection registered", "subscriber", key.String(), "replay_after", cursor)

	go h.writeLoop(conn, cursor)
	go h.heartbeat(conn)
	h.readLoop(conn)
	h.teardown(conn)
}

// handshake reads and verifies the hello frame. On failure the socket is
// closed and ok is false.
func (h *Hub) handshake(ctx context.Context, ws *websocket.Conn) (key SubscriberKey, cursor int64, ok bool) {
	hctx, cancel := context.WithTimeout(ctx, h.handshakeTimeout())
	defer cancel()
	_, data, err := ws.Read(hctx)
	if err != nil {
		h.reject(ws, "no hello frame")
		return SubscriberKey{}, 0, false
	}
	frame, err := ParseClientFrame(data)
	if err != nil || frame.Type != FrameHello {
		h.reject(ws, "first frame is not a valid hello")
		return SubscriberKey{}, 0, false
	}
	hash := HashToken(frame.Token)
	rec, err := h.Tokens.Lookup(hctx, hash)
	switch {
	case errors.Is(err, ErrTokenNotFound):
		h.reject(ws, "unknown token")
		return SubscriberKey{}, 0, false
	case err != nil:
		// A store failure is not an auth verdict: close as an internal
		// error so the shim retries instead of discarding its token.
		h.log().Error("token lookup failed", "err", err)
		_ = ws.Close(websocket.StatusInternalError, "token lookup failed")
		return SubscriberKey{}, 0, false
	}
	h.touchToken(hash)
	key = SubscriberKey{
		UserID:    strconv.FormatInt(rec.GitHubUserID, 10),
		SessionID: frame.SessionID,
	}
	if frame.LastEventID != "" {
		seq, found, err := h.Buffer.SeqOf(hctx, key, frame.LastEventID)
		if err != nil {
			h.log().Error("cursor lookup failed", "subscriber", key.String(), "err", err)
			_ = ws.Close(websocket.StatusInternalError, "cursor lookup failed")
			return SubscriberKey{}, 0, false
		}
		if found {
			cursor = seq
		}
		// Unknown or expired id: cursor stays 0 and the whole buffer
		// replays; the shim dedupes by event id.
	}
	return key, cursor, true
}

// touchToken stamps the token row's last_used asynchronously. Best-effort by
// design: hello never waits on it, and a failure is only logged.
func (h *Hub) touchToken(hash string) {
	if h.Toucher == nil {
		return
	}
	at := h.clock()()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.Toucher.TouchToken(ctx, hash, at); err != nil {
			h.log().Warn("token touch failed", "err", err)
		}
	}()
}

func (h *Hub) reject(ws *websocket.Conn, reason string) {
	h.count(func(m *Metrics) { m.AuthRejected.Add(1) })
	h.log().Info("connection rejected", "reason", reason)
	_ = ws.Close(websocket.StatusCode(CloseUnauthorized), "unauthorized")
}

// readLoop dispatches shim frames until the connection dies. It runs on the
// ServeWS goroutine.
func (h *Hub) readLoop(conn *Conn) {
	for {
		_, data, err := conn.ws.Read(conn.ctx)
		if err != nil {
			return
		}
		frame, err := ParseClientFrame(data)
		if err != nil || frame.Type == FrameHello {
			conn.shutdown(websocket.StatusProtocolError, "protocol error")
			return
		}
		switch frame.Type {
		case FrameSubscribe, FrameUnsubscribe:
			ref := PRRef{Repo: frame.Repo, PR: frame.PR}
			op := h.Subs.Subscribe
			if frame.Type == FrameUnsubscribe {
				op = h.Subs.Unsubscribe
			}
			if err := op(conn.ctx, ref, conn.Key); err != nil {
				// The shim believes the (un)subscribe took effect; a
				// silent failure would wedge it, so force a reconnect.
				h.log().Error("subscription write failed", "subscriber", conn.Key.String(), "pr", ref.String(), "err", err)
				conn.shutdown(websocket.StatusInternalError, "subscription write failed")
				return
			}
			h.log().Info(frame.Type, "subscriber", conn.Key.String(), "pr", ref.String())
		case FrameAck:
			if err := h.Buffer.Ack(conn.ctx, conn.Key, frame.ID); err != nil {
				// Non-fatal: the row lingers until its TTL.
				h.log().Warn("ack failed", "subscriber", conn.Key.String(), "id", frame.ID, "err", err)
				continue
			}
			h.count(func(m *Metrics) { m.EventsAcked.Add(1); m.BufferDepth.Add(-1) })
		}
	}
}

// writeLoop is the connection's single data writer: drain buffered events
// past the cursor, then sleep until nudged. The first drain is the replay.
func (h *Hub) writeLoop(conn *Conn, cursor int64) {
	replay := true
	for {
		events, err := h.Buffer.After(conn.ctx, conn.Key, cursor)
		if err != nil {
			if conn.ctx.Err() == nil {
				h.log().Error("buffer read failed", "subscriber", conn.Key.String(), "err", err)
			}
			conn.shutdown(websocket.StatusInternalError, "buffer read failed")
			return
		}
		if replay && len(events) > 0 {
			h.count(func(m *Metrics) { m.ReplaySessions.Add(1); m.ReplayEvents.Add(int64(len(events))) })
		}
		for _, ev := range events {
			data, err := ev.Encode()
			if err != nil {
				h.log().Error("encode event failed", "id", ev.ID, "err", err)
				cursor = ev.Seq
				continue
			}
			if err := conn.ws.Write(conn.ctx, websocket.MessageText, data); err != nil {
				conn.shutdown(websocket.StatusInternalError, "write failed")
				return
			}
			cursor = ev.Seq
			h.count(func(m *Metrics) { m.EventsPushed.Add(1) })
		}
		replay = false
		select {
		case <-conn.nudge:
		case <-conn.done():
			return
		}
	}
}

// heartbeat pings the shim on an interval and refreshes the durable
// presence row occasionally. A failed ping declares the peer dead. It must
// only run while readLoop does — pongs are processed by the reader.
func (h *Hub) heartbeat(conn *Conn) {
	interval := h.Heartbeat
	if interval <= 0 {
		interval = DefaultHeartbeat
	}
	pingTimeout := h.PingTimeout
	if pingTimeout <= 0 {
		pingTimeout = DefaultPingTimeout
	}
	touchEvery := h.TouchInterval
	if touchEvery <= 0 {
		touchEvery = DefaultTouchInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastTouch := h.clock()()
	for {
		select {
		case <-conn.done():
			return
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(conn.ctx, pingTimeout)
			err := conn.ws.Ping(pctx)
			cancel()
			if err != nil {
				if conn.ctx.Err() != nil {
					return
				}
				h.count(func(m *Metrics) { m.HeartbeatFailures.Add(1) })
				h.log().Info("heartbeat failed", "subscriber", conn.Key.String(), "err", err)
				conn.shutdown(websocket.StatusGoingAway, "heartbeat timeout")
				return
			}
			if now := h.clock()(); now.Sub(lastTouch) >= touchEvery {
				if err := h.Presence.Touch(conn.ctx, conn.Key, now); err != nil {
					h.log().Warn("presence touch failed", "subscriber", conn.Key.String(), "err", err)
				} else {
					lastTouch = now
				}
			}
		}
	}
}

// teardown runs when the read loop exits: close, unregister, and mark the
// subscriber disconnected — but only if this connection is still the
// registered one, so a replaced connection never disturbs its successor.
func (h *Hub) teardown(conn *Conn) {
	conn.shutdown(websocket.StatusNormalClosure, "")
	h.count(func(m *Metrics) { m.ConnectionsLive.Add(-1) })
	if !h.Reg().Unregister(conn.Key, conn) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(conn.ctx), 5*time.Second)
	defer cancel()
	if err := h.Presence.MarkDisconnected(ctx, conn.Key, h.clock()()); err != nil {
		h.log().Warn("mark disconnected failed", "subscriber", conn.Key.String(), "err", err)
	}
	h.log().Info("connection closed", "subscriber", conn.Key.String())
}

// Deliver fans one worker event out to the PR's subscribers with
// write-then-push semantics: suppression check, buffer append (transactional
// with the event_id dedupe marker), then a nudge to the live connection if
// any. A pr_closed event additionally removes every subscription for the PR
// — after buffering, so offline subscribers still replay the final event.
// Per-subscriber failures don't stop the fan-out; the joined error tells the
// worker to retry (the dedupe marker absorbs the overlap).
func (h *Hub) Deliver(ctx context.Context, req DeliverRequest) (DeliverResult, error) {
	ref := PRRef{Repo: req.Repo, PR: req.PR}
	subs, err := h.Subs.Subscribers(ctx, ref)
	if err != nil {
		return DeliverResult{}, err
	}
	res := DeliverResult{Subscribers: len(subs)}
	var errs []error
	ev := req.Event()
	for _, sub := range subs {
		if req.Suppressed(sub.UserID) {
			res.Suppressed++
			h.count(func(m *Metrics) { m.EventsSuppressed.Add(1) })
			continue
		}
		start := h.clock()()
		_, dup, err := h.Buffer.Append(ctx, sub, ev)
		if err != nil {
			h.log().Error("buffer append failed", "subscriber", sub.String(), "id", req.EventID, "err", err)
			errs = append(errs, err)
			continue
		}
		elapsed := h.clock()().Sub(start).Milliseconds()
		h.count(func(m *Metrics) {
			m.DeliverLatencySumMS.Add(elapsed)
			m.DeliverLatencyCount.Add(1)
		})
		if dup {
			res.Deduped++
			h.count(func(m *Metrics) { m.EventsDeduped.Add(1) })
			continue
		}
		res.Buffered++
		h.count(func(m *Metrics) { m.EventsBuffered.Add(1); m.BufferDepth.Add(1) })
		if conn, ok := h.Reg().Get(sub); ok {
			conn.Nudge()
			res.Pushed++
		}
	}
	if req.Kind == KindPRClosed {
		if err := h.Subs.RemoveAllForPR(ctx, ref); err != nil {
			h.log().Error("pr_closed cleanup failed", "pr", ref.String(), "err", err)
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

// Drain closes every live connection with the going-away code — the shim's
// signal to reconnect after the deploy — and waits for their goroutines,
// bounded by ctx. New connections are refused once draining starts.
func (h *Hub) Drain(ctx context.Context) {
	h.draining.Store(true)
	for _, conn := range h.Reg().Snapshot() {
		conn.shutdown(websocket.StatusGoingAway, "gateway draining")
	}
	done := make(chan struct{})
	go func() {
		h.conns.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		h.log().Warn("drain timed out", "remaining", h.Reg().Len())
	}
}

func (h *Hub) handshakeTimeout() time.Duration {
	if h.HandshakeTimeout > 0 {
		return h.HandshakeTimeout
	}
	return DefaultHandshakeTimeout
}

func (h *Hub) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *Hub) clock() func() time.Time {
	if h.Now != nil {
		return h.Now
	}
	return time.Now
}

func (h *Hub) count(f func(*Metrics)) {
	if h.Metrics != nil {
		f(h.Metrics)
	}
}
