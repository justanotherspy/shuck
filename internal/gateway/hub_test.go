package gateway

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// newTestHub wires a Hub to fresh fakes.
func newTestHub() (*Hub, *fakeTokens, *fakeSubs, *fakeBuffer, *fakePresence) {
	tokens := newFakeTokens()
	subs := newFakeSubs()
	buffer := newFakeBuffer()
	presence := newFakePresence()
	hub := &Hub{
		Tokens:   tokens,
		Subs:     subs,
		Buffer:   buffer,
		Presence: presence,
		Log:      slog.New(slog.DiscardHandler),
		Metrics:  &Metrics{},
	}
	return hub, tokens, subs, buffer, presence
}

func subscribeBoth(t *testing.T, subs *fakeSubs, ref PRRef, keys ...SubscriberKey) {
	t.Helper()
	for _, key := range keys {
		if err := subs.Subscribe(context.Background(), ref, key); err != nil {
			t.Fatalf("seed subscribe: %v", err)
		}
	}
}

func TestDeliverFanOutBuffersAndNudges(t *testing.T) {
	hub, _, subs, buffer, _ := newTestHub()
	ref := PRRef{Repo: "octo/repo", PR: 7}
	keyA := SubscriberKey{UserID: "1", SessionID: "sa"}
	keyB := SubscriberKey{UserID: "2", SessionID: "sb"}
	subscribeBoth(t, subs, ref, keyA, keyB)

	// Only A is connected.
	connA := newConn(context.Background(), keyA, nil)
	hub.Reg().Register(keyA, connA)

	res, err := hub.Deliver(context.Background(), validDeliver())
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if res.Subscribers != 2 || res.Buffered != 2 || res.Pushed != 1 {
		t.Fatalf("result = %+v, want 2 subscribers, 2 buffered, 1 pushed", res)
	}
	if buffer.depth(keyA) != 1 || buffer.depth(keyB) != 1 {
		t.Fatalf("buffer depths = %d/%d, want 1/1", buffer.depth(keyA), buffer.depth(keyB))
	}
	select {
	case <-connA.nudge:
	default:
		t.Fatal("connected subscriber was not nudged")
	}
	if hub.Metrics.EventsBuffered.Load() != 2 || hub.Metrics.BufferDepth.Load() != 2 {
		t.Fatalf("metrics buffered=%d depth=%d, want 2/2",
			hub.Metrics.EventsBuffered.Load(), hub.Metrics.BufferDepth.Load())
	}
}

func TestDeliverSuppressionIsKindScoped(t *testing.T) {
	hub, _, subs, buffer, _ := newTestHub()
	ref := PRRef{Repo: "octo/repo", PR: 7}
	author := SubscriberKey{UserID: "42", SessionID: "sa"} // matches validDeliver's author
	other := SubscriberKey{UserID: "7", SessionID: "sb"}
	subscribeBoth(t, subs, ref, author, other)

	review := validDeliver()
	review.Kind = KindReviewComment
	res, err := hub.Deliver(context.Background(), review)
	if err != nil {
		t.Fatalf("Deliver review_comment: %v", err)
	}
	if res.Suppressed != 1 || res.Buffered != 1 {
		t.Fatalf("review result = %+v, want 1 suppressed, 1 buffered", res)
	}
	if buffer.depth(author) != 0 {
		t.Fatal("self-authored review was buffered for its author")
	}
	if buffer.depth(other) != 1 {
		t.Fatal("review not buffered for the other subscriber")
	}

	ci := validDeliver() // ci_failure by the same author
	ci.EventID = "ev-2"
	res, err = hub.Deliver(context.Background(), ci)
	if err != nil {
		t.Fatalf("Deliver ci_failure: %v", err)
	}
	if res.Suppressed != 0 || res.Buffered != 2 {
		t.Fatalf("ci result = %+v, want 0 suppressed, 2 buffered", res)
	}
	if buffer.depth(author) != 1 {
		t.Fatal("ci_failure suppressed for its author — it must never be")
	}
}

func TestDeliverDedupesOnEventID(t *testing.T) {
	hub, _, subs, buffer, _ := newTestHub()
	ref := PRRef{Repo: "octo/repo", PR: 7}
	key := SubscriberKey{UserID: "1", SessionID: "sa"}
	subscribeBoth(t, subs, ref, key)

	req := validDeliver()
	if _, err := hub.Deliver(context.Background(), req); err != nil {
		t.Fatalf("first Deliver: %v", err)
	}
	res, err := hub.Deliver(context.Background(), req) // worker retry
	if err != nil {
		t.Fatalf("retry Deliver: %v", err)
	}
	if res.Deduped != 1 || res.Buffered != 0 {
		t.Fatalf("retry result = %+v, want 1 deduped, 0 buffered", res)
	}
	if buffer.depth(key) != 1 {
		t.Fatalf("buffer depth = %d after retry, want 1", buffer.depth(key))
	}
}

func TestDeliverPRClosedRemovesSubscriptionsAfterBuffering(t *testing.T) {
	hub, _, subs, buffer, _ := newTestHub()
	ref := PRRef{Repo: "octo/repo", PR: 7}
	key := SubscriberKey{UserID: "1", SessionID: "sa"}
	subscribeBoth(t, subs, ref, key)

	req := validDeliver()
	req.Kind = KindPRClosed
	req.Summary = "PR merged"
	res, err := hub.Deliver(context.Background(), req)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if res.Buffered != 1 {
		t.Fatalf("final event not buffered: %+v", res)
	}
	if subs.count(ref) != 0 {
		t.Fatalf("subscriptions remain after pr_closed: %d", subs.count(ref))
	}
	if buffer.depth(key) != 1 {
		t.Fatal("offline subscriber lost the final informational event")
	}
	// Buffering must precede the subscription removal so a crash between
	// the two just re-runs cleanup on retry.
	log := buffer.opLog()
	if len(log) == 0 || log[0] != "append:"+key.String()+":"+req.EventID {
		t.Fatalf("op log = %v, want the append first", log)
	}
}

func TestDeliverPartialFailureContinuesAndErrors(t *testing.T) {
	hub, _, subs, buffer, _ := newTestHub()
	ref := PRRef{Repo: "octo/repo", PR: 7}
	subscribeBoth(t, subs, ref, SubscriberKey{UserID: "1", SessionID: "sa"})
	buffer.appendErr = errFake

	res, err := hub.Deliver(context.Background(), validDeliver())
	if err == nil {
		t.Fatal("Deliver swallowed the append failure — the worker would never retry")
	}
	if res.Buffered != 0 {
		t.Fatalf("result = %+v, want nothing buffered", res)
	}
}

func TestDeliverSubscriberListingFailure(t *testing.T) {
	hub, _, subs, _, _ := newTestHub()
	subs.err = errFake
	if _, err := hub.Deliver(context.Background(), validDeliver()); err == nil {
		t.Fatal("Deliver ignored a subscriber listing failure")
	}
}

func TestDrainWithoutConnectionsReturns(t *testing.T) {
	hub, _, _, _, _ := newTestHub()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	hub.Drain(ctx)
	if !hub.Draining() {
		t.Fatal("Draining() false after Drain")
	}
}
