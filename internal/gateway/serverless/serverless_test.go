package serverless

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/gateway"
)

// harness bundles a Gateway with all its fakes.
type harness struct {
	g        *Gateway
	tokens   *fakeTokens
	toucher  *fakeToucher
	subs     *fakeSubs
	buffer   *fakeBuffer
	presence *fakePresence
	registry *fakeRegistry
	conns    *fakeConns
	metrics  *gateway.Metrics
}

func newHarness() *harness {
	h := &harness{
		tokens:   newFakeTokens(),
		toucher:  &fakeToucher{},
		subs:     newFakeSubs(),
		buffer:   newFakeBuffer(),
		presence: newFakePresence(),
		registry: newFakeRegistry(),
		conns:    newFakeConns(),
		metrics:  &gateway.Metrics{},
	}
	h.g = &Gateway{
		Tokens:   h.tokens,
		Subs:     h.subs,
		Buffer:   h.buffer,
		Presence: h.presence,
		Registry: h.registry,
		Conns:    h.conns,
		Toucher:  h.toucher,
		Metrics:  h.metrics,
		Now:      func() time.Time { return time.Unix(1000, 0) },
	}
	return h
}

const (
	testToken = "shk_test-token"
	testConn  = "conn-1"
)

var testSub = gateway.SubscriberKey{UserID: "42", SessionID: "sess-1"}

func (h *harness) seedToken() {
	h.tokens.add(testToken, gateway.TokenRecord{GitHubUserID: 42, GitHubLogin: "octocat"})
}

// helloFrame builds a valid hello frame body.
func helloFrame(token, session string) []byte {
	data, err := gateway.ClientFrame{Type: gateway.FrameHello, Token: token, SessionID: session}.Encode()
	if err != nil {
		panic(err)
	}
	return data
}

// connect performs a successful hello for testSub on testConn.
func (h *harness) connect(t *testing.T) {
	t.Helper()
	h.seedToken()
	if err := h.g.Message(context.Background(), testConn, helloFrame(testToken, "sess-1")); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if _, ok := h.registry.forward[testSub]; !ok {
		t.Fatal("hello did not register the connection")
	}
}

func frame(t *testing.T, f gateway.ClientFrame) []byte {
	t.Helper()
	data, err := f.Encode()
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	return data
}

func TestHelloRegistersAndReplays(t *testing.T) {
	h := newHarness()
	h.seedToken()
	// Two buffered events await replay; a third was acked away already.
	for _, id := range []string{"ev-1", "ev-2"} {
		if _, _, err := h.buffer.Append(context.Background(), testSub, gateway.Event{ID: id, Repo: "octo/repo", PR: 7, Kind: gateway.KindCIFailure}); err != nil {
			t.Fatal(err)
		}
	}

	if err := h.g.Message(context.Background(), testConn, helloFrame(testToken, "sess-1")); err != nil {
		t.Fatalf("hello: %v", err)
	}

	if got := h.registry.forward[testSub]; got != testConn {
		t.Fatalf("registered conn = %q, want %q", got, testConn)
	}
	sent := h.conns.sent(testConn)
	if len(sent) != 2 || !strings.Contains(sent[0], "ev-1") || !strings.Contains(sent[1], "ev-2") {
		t.Fatalf("replay frames = %v, want ev-1 then ev-2", sent)
	}
	if _, ok := h.presence.lastSeen[testSub]; !ok {
		t.Fatal("presence not touched on hello")
	}
	if len(h.toucher.hashes) != 1 || h.toucher.hashes[0] != gateway.HashToken(testToken) {
		t.Fatalf("token touch hashes = %v", h.toucher.hashes)
	}
	if got := h.metrics.ReplayEvents.Load(); got != 2 {
		t.Fatalf("ReplayEvents = %d, want 2", got)
	}
}

func TestHelloUnknownTokenSendsUnauthorized(t *testing.T) {
	h := newHarness()
	if err := h.g.Message(context.Background(), testConn, helloFrame("shk_wrong", "sess-1")); err != nil {
		t.Fatalf("hello: %v", err)
	}
	sent := h.conns.sent(testConn)
	if len(sent) != 1 || !strings.Contains(sent[0], FrameUnauthorized) {
		t.Fatalf("frames = %v, want one unauthorized frame", sent)
	}
	if !slices.Contains(h.conns.closedConns(), testConn) {
		t.Fatal("connection not closed after rejection")
	}
	if len(h.registry.forward) != 0 {
		t.Fatal("rejected hello must not register")
	}
	if got := h.metrics.AuthRejected.Load(); got != 1 {
		t.Fatalf("AuthRejected = %d, want 1", got)
	}
}

func TestHelloTokenStoreErrorDropsWithoutVerdict(t *testing.T) {
	h := newHarness()
	h.tokens.err = errFake
	if err := h.g.Message(context.Background(), testConn, helloFrame(testToken, "sess-1")); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if sent := h.conns.sent(testConn); len(sent) != 0 {
		t.Fatalf("frames = %v, want none (a store failure is not an auth verdict)", sent)
	}
	if !slices.Contains(h.conns.closedConns(), testConn) {
		t.Fatal("connection not closed on store failure")
	}
}

func TestHelloRegistrySetErrorDrops(t *testing.T) {
	h := newHarness()
	h.seedToken()
	h.registry.setErr = errFake
	if err := h.g.Message(context.Background(), testConn, helloFrame(testToken, "sess-1")); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if !slices.Contains(h.conns.closedConns(), testConn) {
		t.Fatal("connection not closed on registry failure")
	}
}

func TestHelloNewestWinsReplacesOlderConnection(t *testing.T) {
	h := newHarness()
	h.connect(t)

	if err := h.g.Message(context.Background(), "conn-2", helloFrame(testToken, "sess-1")); err != nil {
		t.Fatalf("second hello: %v", err)
	}
	if got := h.registry.forward[testSub]; got != "conn-2" {
		t.Fatalf("registered conn = %q, want conn-2", got)
	}
	sent := h.conns.sent(testConn)
	if len(sent) != 1 || !strings.Contains(sent[0], FrameReplaced) {
		t.Fatalf("old conn frames = %v, want one replaced frame", sent)
	}
	if !slices.Contains(h.conns.closedConns(), testConn) {
		t.Fatal("old connection not closed")
	}
	if got := h.metrics.ConnectionsReplaced.Load(); got != 1 {
		t.Fatalf("ConnectionsReplaced = %d, want 1", got)
	}

	// The replaced connection's disconnect must not disturb the successor.
	h.g.Disconnect(context.Background(), testConn)
	if got := h.registry.forward[testSub]; got != "conn-2" {
		t.Fatalf("after old disconnect, registered conn = %q, want conn-2", got)
	}
}

func TestMessageBadFrameCloses(t *testing.T) {
	h := newHarness()
	for _, body := range []string{"not json", `{"type":"bogus"}`, `{"type":"subscribe"}`} {
		h.conns.closed = nil
		if err := h.g.Message(context.Background(), testConn, []byte(body)); err != nil {
			t.Fatalf("message %q: %v", body, err)
		}
		if !slices.Contains(h.conns.closedConns(), testConn) {
			t.Fatalf("body %q did not close the connection", body)
		}
	}
}

func TestMessageBeforeHelloCloses(t *testing.T) {
	h := newHarness()
	body := frame(t, gateway.ClientFrame{Type: gateway.FrameSubscribe, Repo: "octo/repo", PR: 7})
	if err := h.g.Message(context.Background(), testConn, body); err != nil {
		t.Fatalf("message: %v", err)
	}
	if !slices.Contains(h.conns.closedConns(), testConn) {
		t.Fatal("frame before hello did not close the connection")
	}
	if len(h.subs.ops) != 0 {
		t.Fatalf("subscription ops = %v, want none", h.subs.ops)
	}
}

func TestMessageRegistryLookupErrorCloses(t *testing.T) {
	h := newHarness()
	h.registry.lookupErr = errFake
	body := frame(t, gateway.ClientFrame{Type: gateway.FramePing})
	if err := h.g.Message(context.Background(), testConn, body); err != nil {
		t.Fatalf("message: %v", err)
	}
	if !slices.Contains(h.conns.closedConns(), testConn) {
		t.Fatal("registry lookup failure did not close the connection")
	}
}

func TestSubscribeUnsubscribe(t *testing.T) {
	h := newHarness()
	h.connect(t)
	ref := gateway.PRRef{Repo: "octo/repo", PR: 7}

	if err := h.g.Message(context.Background(), testConn, frame(t, gateway.ClientFrame{Type: gateway.FrameSubscribe, Repo: "octo/repo", PR: 7})); err != nil {
		t.Fatal(err)
	}
	if !h.subs.subs[ref][testSub] {
		t.Fatal("subscribe did not record the subscription")
	}
	if err := h.g.Message(context.Background(), testConn, frame(t, gateway.ClientFrame{Type: gateway.FrameUnsubscribe, Repo: "octo/repo", PR: 7})); err != nil {
		t.Fatal(err)
	}
	if h.subs.subs[ref][testSub] {
		t.Fatal("unsubscribe did not remove the subscription")
	}
	if slices.Contains(h.conns.closedConns(), testConn) {
		t.Fatal("healthy subscribe/unsubscribe closed the connection")
	}
}

func TestSubscribeStoreErrorCloses(t *testing.T) {
	h := newHarness()
	h.connect(t)
	h.subs.err = errFake
	if err := h.g.Message(context.Background(), testConn, frame(t, gateway.ClientFrame{Type: gateway.FrameSubscribe, Repo: "octo/repo", PR: 7})); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.conns.closedConns(), testConn) {
		t.Fatal("subscription store failure did not close the connection")
	}
}

func TestAckDeletesBufferRow(t *testing.T) {
	h := newHarness()
	h.connect(t)
	if _, _, err := h.buffer.Append(context.Background(), testSub, gateway.Event{ID: "ev-1", Repo: "octo/repo", PR: 7, Kind: gateway.KindCIFailure}); err != nil {
		t.Fatal(err)
	}
	if err := h.g.Message(context.Background(), testConn, frame(t, gateway.ClientFrame{Type: gateway.FrameAck, ID: "ev-1"})); err != nil {
		t.Fatal(err)
	}
	if got := len(h.buffer.events[testSub.String()]); got != 0 {
		t.Fatalf("buffered events after ack = %d, want 0", got)
	}
	if got := h.metrics.EventsAcked.Load(); got != 1 {
		t.Fatalf("EventsAcked = %d, want 1", got)
	}
}

func TestAckErrorIsNonFatal(t *testing.T) {
	h := newHarness()
	h.connect(t)
	h.buffer.ackErr = errFake
	if err := h.g.Message(context.Background(), testConn, frame(t, gateway.ClientFrame{Type: gateway.FrameAck, ID: "ev-1"})); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(h.conns.closedConns(), testConn) {
		t.Fatal("ack failure must not close the connection")
	}
}

func TestPingTouchesPresence(t *testing.T) {
	h := newHarness()
	h.connect(t)
	delete(h.presence.lastSeen, testSub)
	if err := h.g.Message(context.Background(), testConn, frame(t, gateway.ClientFrame{Type: gateway.FramePing})); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.presence.lastSeen[testSub]; !ok {
		t.Fatal("ping did not touch presence")
	}
	if slices.Contains(h.conns.closedConns(), testConn) {
		t.Fatal("ping closed the connection")
	}
}

func TestConnectAcceptsAndCounts(t *testing.T) {
	h := newHarness()
	if err := h.g.Connect(context.Background(), testConn); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got := h.metrics.ConnectionsTotal.Load(); got != 1 {
		t.Fatalf("ConnectionsTotal = %d, want 1", got)
	}
}

func TestDisconnectCleansUp(t *testing.T) {
	h := newHarness()
	h.connect(t)
	h.g.Disconnect(context.Background(), testConn)
	if _, ok := h.registry.forward[testSub]; ok {
		t.Fatal("disconnect left the forward mapping")
	}
	if _, ok := h.presence.disconnected[testSub]; !ok {
		t.Fatal("disconnect did not mark presence")
	}
}

func TestDisconnectUnknownConnectionIsNoop(t *testing.T) {
	h := newHarness()
	h.g.Disconnect(context.Background(), "never-helloed")
	if len(h.presence.disconnected) != 0 {
		t.Fatal("unknown disconnect marked presence")
	}
}

func deliverReq(kind gateway.EventKind, author *gateway.Author) gateway.DeliverRequest {
	return gateway.DeliverRequest{
		Schema:  gateway.DeliverSchema,
		EventID: "guid-1",
		Kind:    kind,
		Repo:    "octo/repo",
		PR:      7,
		Summary: "summary",
		Author:  author,
	}
}

func TestDeliverBuffersAndPushesLiveSubscriber(t *testing.T) {
	h := newHarness()
	h.connect(t)
	ref := gateway.PRRef{Repo: "octo/repo", PR: 7}
	offline := gateway.SubscriberKey{UserID: "43", SessionID: "sess-2"}
	for _, sub := range []gateway.SubscriberKey{testSub, offline} {
		if err := h.subs.Subscribe(context.Background(), ref, sub); err != nil {
			t.Fatal(err)
		}
	}

	res, err := h.g.Deliver(context.Background(), deliverReq(gateway.KindCIFailure, nil))
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if res.Subscribers != 2 || res.Buffered != 2 || res.Pushed != 1 {
		t.Fatalf("result = %+v, want 2 subscribers, 2 buffered, 1 pushed", res)
	}
	sent := h.conns.sent(testConn)
	if len(sent) != 1 || !strings.Contains(sent[0], "guid-1") {
		t.Fatalf("live conn frames = %v, want the delivered event", sent)
	}
	if got := len(h.buffer.events[offline.String()]); got != 1 {
		t.Fatalf("offline buffered = %d, want 1", got)
	}
}

func TestDeliverDedupesRetries(t *testing.T) {
	h := newHarness()
	ref := gateway.PRRef{Repo: "octo/repo", PR: 7}
	if err := h.subs.Subscribe(context.Background(), ref, testSub); err != nil {
		t.Fatal(err)
	}
	if _, err := h.g.Deliver(context.Background(), deliverReq(gateway.KindCIFailure, nil)); err != nil {
		t.Fatal(err)
	}
	res, err := h.g.Deliver(context.Background(), deliverReq(gateway.KindCIFailure, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Deduped != 1 || res.Buffered != 0 {
		t.Fatalf("result = %+v, want 1 deduped, 0 buffered", res)
	}
}

func TestDeliverSuppressesSelfAuthoredReviewKinds(t *testing.T) {
	h := newHarness()
	ref := gateway.PRRef{Repo: "octo/repo", PR: 7}
	if err := h.subs.Subscribe(context.Background(), ref, testSub); err != nil {
		t.Fatal(err)
	}
	author := &gateway.Author{GitHubUserID: 42, Login: "octocat"}

	res, err := h.g.Deliver(context.Background(), deliverReq(gateway.KindReviewComment, author))
	if err != nil {
		t.Fatal(err)
	}
	if res.Suppressed != 1 || res.Buffered != 0 {
		t.Fatalf("review result = %+v, want 1 suppressed", res)
	}

	// ci_failure is never suppressed, whoever caused it.
	res, err = h.g.Deliver(context.Background(), deliverReq(gateway.KindCIFailure, author))
	if err != nil {
		t.Fatal(err)
	}
	if res.Suppressed != 0 || res.Buffered != 1 {
		t.Fatalf("ci result = %+v, want 1 buffered, 0 suppressed", res)
	}
}

func TestDeliverPRClosedRemovesSubscriptionsAfterBuffering(t *testing.T) {
	h := newHarness()
	ref := gateway.PRRef{Repo: "octo/repo", PR: 7}
	if err := h.subs.Subscribe(context.Background(), ref, testSub); err != nil {
		t.Fatal(err)
	}
	res, err := h.g.Deliver(context.Background(), deliverReq(gateway.KindPRClosed, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Buffered != 1 {
		t.Fatalf("result = %+v, want the final event buffered", res)
	}
	if len(h.subs.subs[ref]) != 0 {
		t.Fatal("pr_closed did not remove the PR's subscriptions")
	}
}

func TestDeliverAppendErrorJoinsAndContinues(t *testing.T) {
	h := newHarness()
	ref := gateway.PRRef{Repo: "octo/repo", PR: 7}
	if err := h.subs.Subscribe(context.Background(), ref, testSub); err != nil {
		t.Fatal(err)
	}
	h.buffer.appendErr = errFake
	res, err := h.g.Deliver(context.Background(), deliverReq(gateway.KindCIFailure, nil))
	if err == nil {
		t.Fatal("deliver with failing buffer returned nil error (worker must retry)")
	}
	if res.Buffered != 0 {
		t.Fatalf("result = %+v, want nothing buffered", res)
	}
}

func TestDeliverGoneConnectionCleansUp(t *testing.T) {
	h := newHarness()
	h.connect(t)
	ref := gateway.PRRef{Repo: "octo/repo", PR: 7}
	if err := h.subs.Subscribe(context.Background(), ref, testSub); err != nil {
		t.Fatal(err)
	}
	h.conns.gone[testConn] = true

	res, err := h.g.Deliver(context.Background(), deliverReq(gateway.KindCIFailure, nil))
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if res.Buffered != 1 {
		t.Fatalf("result = %+v, want the event buffered despite the gone conn", res)
	}
	if _, ok := h.registry.forward[testSub]; ok {
		t.Fatal("gone connection left its registry mapping")
	}
	if _, ok := h.presence.disconnected[testSub]; !ok {
		t.Fatal("gone connection not marked disconnected")
	}
	// The buffered row survives for the next reconnect's replay.
	if got := len(h.buffer.events[testSub.String()]); got != 1 {
		t.Fatalf("buffered = %d, want 1", got)
	}
}

func TestDeliverSubscriberListErrorFails(t *testing.T) {
	h := newHarness()
	h.subs.err = errFake
	if _, err := h.g.Deliver(context.Background(), deliverReq(gateway.KindCIFailure, nil)); err == nil {
		t.Fatal("deliver with failing subscriber listing returned nil error")
	}
}

func TestPushDrainsInSeqOrder(t *testing.T) {
	h := newHarness()
	h.connect(t)
	ref := gateway.PRRef{Repo: "octo/repo", PR: 7}
	if err := h.subs.Subscribe(context.Background(), ref, testSub); err != nil {
		t.Fatal(err)
	}
	// A previously buffered-but-unpushed event (e.g. a lost race) must ride
	// along with the next deliver's push.
	if _, _, err := h.buffer.Append(context.Background(), testSub, gateway.Event{ID: "ev-old", Repo: "octo/repo", PR: 7, Kind: gateway.KindCIFailure}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.g.Deliver(context.Background(), deliverReq(gateway.KindCIFailure, nil)); err != nil {
		t.Fatal(err)
	}
	sent := h.conns.sent(testConn)
	if len(sent) != 2 || !strings.Contains(sent[0], "ev-old") || !strings.Contains(sent[1], "guid-1") {
		t.Fatalf("frames = %v, want ev-old then guid-1", sent)
	}
}

// TestSweeperCompatibility exercises the resident gateway.Sweeper over the
// serverless fakes: ping-refreshed subscribers survive, stale ones lose
// subscriptions, buffer, and presence — the exact configuration
// cmd/shuck-gateway's sweep role runs.
func TestSweeperCompatibility(t *testing.T) {
	h := newHarness()
	now := time.Unix(100_000, 0)
	stale := gateway.SubscriberKey{UserID: "9", SessionID: "old"}
	ref := gateway.PRRef{Repo: "octo/repo", PR: 7}
	for _, sub := range []gateway.SubscriberKey{testSub, stale} {
		if err := h.subs.Subscribe(context.Background(), ref, sub); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.presence.Touch(context.Background(), testSub, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := h.presence.Touch(context.Background(), stale, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	sweeper := &gateway.Sweeper{
		Subs:     h.subs,
		Buffer:   h.buffer,
		Presence: h.presence,
		Registry: gateway.NewMemRegistry(),
		Now:      func() time.Time { return now },
	}
	sweeper.Sweep(context.Background())

	if h.subs.subs[ref][stale] {
		t.Fatal("stale subscriber kept its subscription")
	}
	if !h.subs.subs[ref][testSub] {
		t.Fatal("fresh subscriber lost its subscription")
	}
	if _, ok := h.presence.lastSeen[stale]; ok {
		t.Fatal("stale subscriber kept its presence row")
	}
}
