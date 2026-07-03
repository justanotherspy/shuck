package gateway

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func newTestSweeper() (*Sweeper, *fakeSubs, *fakeBuffer, *fakePresence, Registry) {
	subs := newFakeSubs()
	buffer := newFakeBuffer()
	presence := newFakePresence()
	registry := NewMemRegistry()
	sweeper := &Sweeper{
		Subs:        subs,
		Buffer:      buffer,
		Presence:    presence,
		Registry:    registry,
		GraceWindow: 24 * time.Hour,
		Log:         slog.New(slog.DiscardHandler),
		Metrics:     &Metrics{},
	}
	return sweeper, subs, buffer, presence, registry
}

func TestSweepRemovesStaleSubscribers(t *testing.T) {
	sweeper, subs, buffer, presence, _ := newTestSweeper()
	ctx := context.Background()
	now := time.Now()
	sweeper.Now = func() time.Time { return now }

	stale := SubscriberKey{UserID: "1", SessionID: "old"}
	fresh := SubscriberKey{UserID: "2", SessionID: "new"}
	ref := PRRef{Repo: "octo/repo", PR: 7}
	subscribeBoth(t, subs, ref, stale, fresh)
	if _, _, err := buffer.Append(ctx, stale, Event{ID: "ev-1"}); err != nil {
		t.Fatalf("seed buffer: %v", err)
	}

	// stale disconnected two days ago; fresh an hour ago.
	if err := presence.Touch(ctx, stale, now.Add(-49*time.Hour)); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if err := presence.MarkDisconnected(ctx, stale, now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := presence.Touch(ctx, fresh, now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if err := presence.MarkDisconnected(ctx, fresh, now.Add(-time.Hour)); err != nil {
		t.Fatalf("mark: %v", err)
	}

	sweeper.Sweep(ctx)

	if subs.count(ref) != 1 {
		t.Fatalf("subscriptions after sweep = %d, want only the fresh one", subs.count(ref))
	}
	if buffer.depth(stale) != 0 {
		t.Fatal("stale subscriber's buffer not purged")
	}
	if _, ok := presence.disconnectedAt(stale); ok {
		t.Fatal("stale presence row not deleted")
	}
	if sweeper.Metrics.SweepRemoved.Load() != 1 {
		t.Fatalf("SweepRemoved = %d, want 1", sweeper.Metrics.SweepRemoved.Load())
	}
}

func TestSweepNeverRemovesLiveConnections(t *testing.T) {
	sweeper, subs, buffer, presence, registry := newTestSweeper()
	ctx := context.Background()
	now := time.Now()
	sweeper.Now = func() time.Time { return now }

	// Presence looks ancient (e.g. the gateway crashed without marking
	// disconnects and the row went stale) but the subscriber is live.
	key := SubscriberKey{UserID: "1", SessionID: "s"}
	ref := PRRef{Repo: "octo/repo", PR: 7}
	subscribeBoth(t, subs, ref, key)
	if err := presence.Touch(ctx, key, now.Add(-72*time.Hour)); err != nil {
		t.Fatalf("touch: %v", err)
	}
	registry.Register(key, newConn(ctx, key, nil))

	sweeper.Sweep(ctx)

	if subs.count(ref) != 1 {
		t.Fatal("sweep removed a live subscriber's subscriptions")
	}
	if buffer.opLog() != nil {
		t.Fatalf("sweep touched a live subscriber's buffer: %v", buffer.opLog())
	}
}

func TestSweepListingFailureIsNonFatal(t *testing.T) {
	sweeper, _, _, presence, _ := newTestSweeper()
	presence.err = errFake
	sweeper.Sweep(context.Background()) // must not panic
	if sweeper.Metrics.SweepRemoved.Load() != 0 {
		t.Fatal("sweep removed subscribers despite a listing failure")
	}
}

func TestSweeperRunStopsOnContext(t *testing.T) {
	sweeper, _, _, _, _ := newTestSweeper()
	sweeper.Interval = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		sweeper.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop when its context ended")
	}
}
