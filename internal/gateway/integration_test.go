package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The integration suite runs the ticket's acceptance criteria against a
// full Hub behind httptest, dialed by a real WebSocket client.

func startGateway(t *testing.T, hub *Hub) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", hub.ServeWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		hub.Drain(ctx)
		srv.Close()
	})
	return srv
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
}

func dial(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, wsURL(srv), nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func send(t *testing.T, c *websocket.Conn, frame ClientFrame) {
	t.Helper()
	data, err := frame.Encode()
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func hello(t *testing.T, srv *httptest.Server, token, session, lastEventID string) *websocket.Conn {
	t.Helper()
	c := dial(t, srv)
	send(t, c, ClientFrame{Type: FrameHello, Token: token, SessionID: session, LastEventID: lastEventID})
	return c
}

// readEvent reads one event frame, failing the test on timeout.
func readEvent(t *testing.T, c *websocket.Conn) Event {
	t.Helper()
	ev, err := tryReadEvent(c, 2*time.Second)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	return ev
}

func tryReadEvent(c *websocket.Conn, timeout time.Duration) (Event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		return Event{}, err
	}
	var ev Event
	if err := json.Unmarshal(data, &ev); err != nil {
		return Event{}, err
	}
	return ev, nil
}

// expectSilence asserts no frame arrives within the window. The expired
// read context closes the connection (a coder/websocket property), so this
// must be the last operation on c.
func expectSilence(t *testing.T, c *websocket.Conn) {
	t.Helper()
	if ev, err := tryReadEvent(c, 200*time.Millisecond); err == nil {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func deliverReq(id string, kind EventKind, author *Author) DeliverRequest {
	return DeliverRequest{
		Schema:  DeliverSchema,
		EventID: id,
		Kind:    kind,
		Repo:    "octo/repo",
		PR:      7,
		Summary: "summary of " + id,
		Author:  author,
	}
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestIntegrationSubscribeDeliverAck(t *testing.T) {
	hub, tokens, subs, buffer, _ := newTestHub()
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1, GitHubLogin: "alice"})
	srv := startGateway(t, hub)

	c := hello(t, srv, "tok-a", "sess-1", "")
	defer c.Close(websocket.StatusNormalClosure, "")
	send(t, c, ClientFrame{Type: FrameSubscribe, Repo: "octo/repo", PR: 7})
	key := SubscriberKey{UserID: "1", SessionID: "sess-1"}
	waitFor(t, "subscription", func() bool { return subs.count(PRRef{Repo: "octo/repo", PR: 7}) == 1 })

	if _, err := hub.Deliver(context.Background(), deliverReq("ev-1", KindCIFailure, nil)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	ev := readEvent(t, c)
	if ev.ID != "ev-1" || ev.Seq != 1 || ev.Kind != KindCIFailure || ev.Repo != "octo/repo" || ev.PR != 7 {
		t.Fatalf("event = %+v", ev)
	}

	send(t, c, ClientFrame{Type: FrameAck, ID: "ev-1"})
	waitFor(t, "ack", func() bool { return buffer.depth(key) == 0 })

	// Unsubscribe stops future deliveries.
	send(t, c, ClientFrame{Type: FrameUnsubscribe, Repo: "octo/repo", PR: 7})
	waitFor(t, "unsubscribe", func() bool { return subs.count(PRRef{Repo: "octo/repo", PR: 7}) == 0 })
	if _, err := hub.Deliver(context.Background(), deliverReq("ev-2", KindCIFailure, nil)); err != nil {
		t.Fatalf("Deliver after unsubscribe: %v", err)
	}
	expectSilence(t, c)
}

func TestIntegrationOfflineDeliveryArrivesOnce(t *testing.T) {
	hub, tokens, subs, _, _ := newTestHub()
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1})
	srv := startGateway(t, hub)
	key := SubscriberKey{UserID: "1", SessionID: "sess-1"}
	subscribeBoth(t, subs, PRRef{Repo: "octo/repo", PR: 7}, key)

	// Deliver while the subscriber is offline.
	if _, err := hub.Deliver(context.Background(), deliverReq("ev-1", KindCIFailure, nil)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	c := hello(t, srv, "tok-a", "sess-1", "")
	defer c.Close(websocket.StatusNormalClosure, "")
	if ev := readEvent(t, c); ev.ID != "ev-1" {
		t.Fatalf("replayed event = %+v", ev)
	}

	// Worker retry of the same event_id: buffered once, delivered once.
	res, err := hub.Deliver(context.Background(), deliverReq("ev-1", KindCIFailure, nil))
	if err != nil {
		t.Fatalf("retry Deliver: %v", err)
	}
	if res.Deduped != 1 {
		t.Fatalf("retry result = %+v, want 1 deduped", res)
	}
	expectSilence(t, c)
}

func TestIntegrationRestartReplaysFromCursor(t *testing.T) {
	hub, tokens, subs, buffer, presence := newTestHub()
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1})
	srv := startGateway(t, hub)
	key := SubscriberKey{UserID: "1", SessionID: "sess-1"}
	subscribeBoth(t, subs, PRRef{Repo: "octo/repo", PR: 7}, key)

	c := hello(t, srv, "tok-a", "sess-1", "")
	if _, err := hub.Deliver(context.Background(), deliverReq("ev-1", KindCIFailure, nil)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if ev := readEvent(t, c); ev.ID != "ev-1" {
		t.Fatalf("event = %+v", ev)
	}
	c.Close(websocket.StatusNormalClosure, "")

	// Events delivered while the gateway "restarts" and the shim is away
	// land in the durable buffer.
	if _, err := hub.Deliver(context.Background(), deliverReq("ev-2", KindCIFailure, nil)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, err := hub.Deliver(context.Background(), deliverReq("ev-3", KindCIFailure, nil)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	// A fresh Hub over the same durable stores — the restart.
	hub2 := &Hub{
		Tokens: hub.Tokens, Subs: subs, Buffer: buffer, Presence: presence,
		Log: hub.Log, Metrics: &Metrics{},
	}
	srv2 := startGateway(t, hub2)
	c2 := hello(t, srv2, "tok-a", "sess-1", "ev-1")
	defer c2.Close(websocket.StatusNormalClosure, "")
	first := readEvent(t, c2)
	second := readEvent(t, c2)
	if first.ID != "ev-2" || second.ID != "ev-3" {
		t.Fatalf("replay order = %s, %s; want ev-2, ev-3", first.ID, second.ID)
	}
	if second.Seq <= first.Seq {
		t.Fatalf("seq not increasing: %d then %d", first.Seq, second.Seq)
	}
	expectSilence(t, c2)
	if hub2.Metrics.ReplaySessions.Load() != 1 || hub2.Metrics.ReplayEvents.Load() != 2 {
		t.Fatalf("replay metrics = %d sessions / %d events, want 1/2",
			hub2.Metrics.ReplaySessions.Load(), hub2.Metrics.ReplayEvents.Load())
	}
}

func TestIntegrationUnknownCursorReplaysEverything(t *testing.T) {
	hub, tokens, subs, _, _ := newTestHub()
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1})
	srv := startGateway(t, hub)
	key := SubscriberKey{UserID: "1", SessionID: "sess-1"}
	subscribeBoth(t, subs, PRRef{Repo: "octo/repo", PR: 7}, key)
	if _, err := hub.Deliver(context.Background(), deliverReq("ev-1", KindCIFailure, nil)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	c := hello(t, srv, "tok-a", "sess-1", "never-seen")
	defer c.Close(websocket.StatusNormalClosure, "")
	if ev := readEvent(t, c); ev.ID != "ev-1" {
		t.Fatalf("full replay missing ev-1: %+v", ev)
	}
}

func TestIntegrationSessionIDIsNotACapability(t *testing.T) {
	hub, tokens, subs, _, _ := newTestHub()
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1})
	tokens.add("tok-b", TokenRecord{GitHubUserID: 2})
	srv := startGateway(t, hub)

	// User A's session has a buffered event.
	keyA := SubscriberKey{UserID: "1", SessionID: "sess-1"}
	subscribeBoth(t, subs, PRRef{Repo: "octo/repo", PR: 7}, keyA)
	if _, err := hub.Deliver(context.Background(), deliverReq("ev-1", KindCIFailure, nil)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	// User B presents A's session id with B's own token: the subscriber
	// key is namespaced under B, so nothing of A's is visible.
	c := hello(t, srv, "tok-b", "sess-1", "")
	defer c.Close(websocket.StatusNormalClosure, "")
	expectSilence(t, c)
}

func TestIntegrationRejectsBadHandshake(t *testing.T) {
	hub, tokens, _, _, _ := newTestHub()
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1})
	srv := startGateway(t, hub)

	cases := []struct {
		name  string
		frame ClientFrame
	}{
		{"unknown token", ClientFrame{Type: FrameHello, Token: "wrong", SessionID: "s"}},
		{"not a hello", ClientFrame{Type: FrameAck, ID: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := dial(t, srv)
			defer c.Close(websocket.StatusNormalClosure, "")
			send(t, c, tc.frame)
			_, err := tryReadEvent(c, 2*time.Second)
			if err == nil {
				t.Fatal("expected the connection to close")
			}
			if status := websocket.CloseStatus(err); status != websocket.StatusCode(CloseUnauthorized) {
				t.Fatalf("close status = %v, want %d", status, CloseUnauthorized)
			}
		})
	}
	if got := hub.Metrics.AuthRejected.Load(); got != 2 {
		t.Fatalf("AuthRejected = %d, want 2", got)
	}
}

func TestIntegrationSelfAuthoredSuppression(t *testing.T) {
	hub, tokens, subs, buffer, _ := newTestHub()
	tokens.add("tok-a", TokenRecord{GitHubUserID: 42, GitHubLogin: "alice"})
	tokens.add("tok-b", TokenRecord{GitHubUserID: 7, GitHubLogin: "bob"})
	srv := startGateway(t, hub)

	keyA := SubscriberKey{UserID: "42", SessionID: "sa"}
	keyB := SubscriberKey{UserID: "7", SessionID: "sb"}
	subscribeBoth(t, subs, PRRef{Repo: "octo/repo", PR: 7}, keyA, keyB)
	cA := hello(t, srv, "tok-a", "sa", "")
	defer cA.Close(websocket.StatusNormalClosure, "")
	cB := hello(t, srv, "tok-b", "sb", "")
	defer cB.Close(websocket.StatusNormalClosure, "")

	// Alice comments on the PR: only Bob hears about it.
	author := &Author{GitHubUserID: 42, Login: "alice"}
	if _, err := hub.Deliver(context.Background(), deliverReq("ev-1", KindReviewComment, author)); err != nil {
		t.Fatalf("Deliver review: %v", err)
	}
	if ev := readEvent(t, cB); ev.ID != "ev-1" {
		t.Fatalf("bob's event = %+v", ev)
	}
	if buffer.depth(keyA) != 0 {
		t.Fatal("suppressed event was buffered for its author")
	}

	// CI fails on the same PR: reaches BOTH, author included. Alice's
	// *next* frame being the CI failure proves the review never reached
	// her (frames are ordered per connection).
	if _, err := hub.Deliver(context.Background(), deliverReq("ev-2", KindCIFailure, author)); err != nil {
		t.Fatalf("Deliver ci: %v", err)
	}
	if ev := readEvent(t, cA); ev.ID != "ev-2" {
		t.Fatalf("alice's first event = %+v, want the ci_failure (review must be suppressed)", ev)
	}
	if ev := readEvent(t, cB); ev.ID != "ev-2" {
		t.Fatalf("bob's ci event = %+v", ev)
	}
}

func TestIntegrationPRClosedCleansSubscriptions(t *testing.T) {
	hub, tokens, subs, _, _ := newTestHub()
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1})
	srv := startGateway(t, hub)
	ref := PRRef{Repo: "octo/repo", PR: 7}
	subscribeBoth(t, subs, ref, SubscriberKey{UserID: "1", SessionID: "sess-1"})
	c := hello(t, srv, "tok-a", "sess-1", "")
	defer c.Close(websocket.StatusNormalClosure, "")

	if _, err := hub.Deliver(context.Background(), deliverReq("ev-1", KindPRClosed, nil)); err != nil {
		t.Fatalf("Deliver pr_closed: %v", err)
	}
	if ev := readEvent(t, c); ev.Kind != KindPRClosed {
		t.Fatalf("final event = %+v", ev)
	}
	if subs.count(ref) != 0 {
		t.Fatalf("%d subscriptions remain after pr_closed", subs.count(ref))
	}
	// Nothing further is deliverable for the PR.
	res, err := hub.Deliver(context.Background(), deliverReq("ev-2", KindCIFailure, nil))
	if err != nil {
		t.Fatalf("Deliver after close: %v", err)
	}
	if res.Subscribers != 0 {
		t.Fatalf("post-close deliver found %d subscribers", res.Subscribers)
	}
}

func TestIntegrationNewestConnectionWins(t *testing.T) {
	hub, tokens, subs, _, _ := newTestHub()
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1})
	srv := startGateway(t, hub)
	subscribeBoth(t, subs, PRRef{Repo: "octo/repo", PR: 7}, SubscriberKey{UserID: "1", SessionID: "sess-1"})

	c1 := hello(t, srv, "tok-a", "sess-1", "")
	waitFor(t, "first registration", func() bool { return hub.Reg().Len() == 1 })
	c2 := hello(t, srv, "tok-a", "sess-1", "")
	defer c2.Close(websocket.StatusNormalClosure, "")

	// The first connection is closed with the replaced code.
	_, err := tryReadEvent(c1, 2*time.Second)
	if err == nil {
		t.Fatal("first connection still open after replacement")
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusCode(CloseReplaced) {
		t.Fatalf("close status = %v, want %d", status, CloseReplaced)
	}

	// Deliveries reach only the survivor.
	if _, err := hub.Deliver(context.Background(), deliverReq("ev-1", KindCIFailure, nil)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if ev := readEvent(t, c2); ev.ID != "ev-1" {
		t.Fatalf("survivor's event = %+v", ev)
	}
	if hub.Metrics.ConnectionsReplaced.Load() != 1 {
		t.Fatalf("ConnectionsReplaced = %d, want 1", hub.Metrics.ConnectionsReplaced.Load())
	}
}

func TestIntegrationHeartbeatKeepsHealthyConnAlive(t *testing.T) {
	hub, tokens, _, _, _ := newTestHub()
	hub.Heartbeat = 20 * time.Millisecond
	hub.PingTimeout = 200 * time.Millisecond
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1})
	srv := startGateway(t, hub)

	c := hello(t, srv, "tok-a", "sess-1", "")
	defer c.Close(websocket.StatusNormalClosure, "")
	// A reading client answers pings; CloseRead pumps the control frames.
	readCtx := c.CloseRead(context.Background())
	waitFor(t, "registration", func() bool { return hub.Reg().Len() == 1 })

	time.Sleep(150 * time.Millisecond) // several heartbeat intervals
	if hub.Reg().Len() != 1 {
		t.Fatal("healthy connection was dropped by its own heartbeat")
	}
	if got := hub.Metrics.HeartbeatFailures.Load(); got != 0 {
		t.Fatalf("HeartbeatFailures = %d for a healthy conn", got)
	}
	select {
	case <-readCtx.Done():
		t.Fatal("connection closed unexpectedly")
	default:
	}
}

func TestIntegrationHeartbeatDropsDeadPeer(t *testing.T) {
	hub, tokens, _, _, presence := newTestHub()
	hub.Heartbeat = 20 * time.Millisecond
	hub.PingTimeout = 50 * time.Millisecond
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1})
	srv := startGateway(t, hub)

	// A client that never reads never answers pings — a dead peer as far
	// as the gateway can tell.
	c := hello(t, srv, "tok-a", "sess-1", "")
	defer c.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "registration", func() bool { return hub.Reg().Len() == 1 })

	waitFor(t, "dead peer eviction", func() bool { return hub.Reg().Len() == 0 })
	if hub.Metrics.HeartbeatFailures.Load() == 0 {
		t.Fatal("heartbeat failure not counted")
	}
	key := SubscriberKey{UserID: "1", SessionID: "sess-1"}
	waitFor(t, "disconnect mark", func() bool { _, ok := presence.disconnectedAt(key); return ok })
}

func TestIntegrationDrainSendsGoingAway(t *testing.T) {
	hub, tokens, _, _, presence := newTestHub()
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1})
	srv := startGateway(t, hub)

	c := hello(t, srv, "tok-a", "sess-1", "")
	defer c.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "registration", func() bool { return hub.Reg().Len() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hub.Drain(ctx)

	_, err := tryReadEvent(c, 2*time.Second)
	if err == nil {
		t.Fatal("connection survived the drain")
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusGoingAway {
		t.Fatalf("close status = %v, want going away (1001)", status)
	}
	key := SubscriberKey{UserID: "1", SessionID: "sess-1"}
	if _, ok := presence.disconnectedAt(key); !ok {
		t.Fatal("drained subscriber not marked disconnected")
	}

	// New connections are refused while draining.
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	_, resp, err := websocket.Dial(dialCtx, wsURL(srv), nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("dial succeeded while draining")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("draining dial response = %+v, want 503", resp)
	}
}

func TestIntegrationConcurrentDeliveryDuringReplay(t *testing.T) {
	hub, tokens, subs, _, _ := newTestHub()
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1})
	srv := startGateway(t, hub)
	key := SubscriberKey{UserID: "1", SessionID: "sess-1"}
	subscribeBoth(t, subs, PRRef{Repo: "octo/repo", PR: 7}, key)

	const seeded, live = 25, 25
	for i := range seeded {
		if _, err := hub.Deliver(context.Background(), deliverReq(fmt.Sprintf("seed-%d", i), KindCIFailure, nil)); err != nil {
			t.Fatalf("seed Deliver: %v", err)
		}
	}

	c := hello(t, srv, "tok-a", "sess-1", "")
	defer c.Close(websocket.StatusNormalClosure, "")
	// Deliver concurrently with the replay drain.
	errCh := make(chan error, live)
	for i := range live {
		go func(i int) {
			_, err := hub.Deliver(context.Background(), deliverReq(fmt.Sprintf("live-%d", i), KindCIFailure, nil))
			errCh <- err
		}(i)
	}
	for range live {
		if err := <-errCh; err != nil {
			t.Fatalf("live Deliver: %v", err)
		}
	}

	seen := make(map[string]int)
	lastSeq := int64(0)
	for range seeded + live {
		ev := readEvent(t, c)
		seen[ev.ID]++
		if ev.Seq <= lastSeq {
			t.Fatalf("seq not strictly increasing: %d after %d (id %s)", ev.Seq, lastSeq, ev.ID)
		}
		lastSeq = ev.Seq
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("event %s delivered %d times", id, n)
		}
	}
	expectSilence(t, c)
}

func TestIntegrationAckedEventNotReplayed(t *testing.T) {
	hub, tokens, subs, buffer, _ := newTestHub()
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1})
	srv := startGateway(t, hub)
	key := SubscriberKey{UserID: "1", SessionID: "sess-1"}
	subscribeBoth(t, subs, PRRef{Repo: "octo/repo", PR: 7}, key)

	c := hello(t, srv, "tok-a", "sess-1", "")
	if _, err := hub.Deliver(context.Background(), deliverReq("ev-1", KindCIFailure, nil)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, err := hub.Deliver(context.Background(), deliverReq("ev-2", KindCIFailure, nil)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	readEvent(t, c)
	readEvent(t, c)
	send(t, c, ClientFrame{Type: FrameAck, ID: "ev-1"})
	waitFor(t, "ack", func() bool { return buffer.depth(key) == 1 })
	c.Close(websocket.StatusNormalClosure, "")

	// Reconnect without a cursor: only the unacked event replays.
	c2 := hello(t, srv, "tok-a", "sess-1", "")
	defer c2.Close(websocket.StatusNormalClosure, "")
	if ev := readEvent(t, c2); ev.ID != "ev-2" {
		t.Fatalf("replayed = %+v, want only the unacked ev-2", ev)
	}
	expectSilence(t, c2)
}

// TestIntegrationPingIsIgnored pins the one-shim-two-gateways contract: the
// shim sends an app-level {"type":"ping"} every 5 minutes for the serverless
// gateway's benefit, and the resident hub must tolerate it — the connection
// survives and deliveries still flow afterwards.
func TestIntegrationPingIsIgnored(t *testing.T) {
	hub, tokens, subs, _, _ := newTestHub()
	tokens.add("tok-a", TokenRecord{GitHubUserID: 1, GitHubLogin: "alice"})
	srv := startGateway(t, hub)

	c := hello(t, srv, "tok-a", "sess-1", "")
	defer c.Close(websocket.StatusNormalClosure, "")
	send(t, c, ClientFrame{Type: FramePing})
	send(t, c, ClientFrame{Type: FrameSubscribe, Repo: "octo/repo", PR: 7})
	waitFor(t, "subscription", func() bool { return subs.count(PRRef{Repo: "octo/repo", PR: 7}) == 1 })
	send(t, c, ClientFrame{Type: FramePing})

	if _, err := hub.Deliver(context.Background(), deliverReq("ev-ping-1", KindCIFailure, nil)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	ev := readEvent(t, c)
	if ev.ID != "ev-ping-1" {
		t.Fatalf("event after pings = %+v, want ev-ping-1", ev)
	}
}
