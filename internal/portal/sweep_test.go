package portal

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

func TestSweepRevokesNonMembers(t *testing.T) {
	store := newFakeStore()
	store.rows["h-member"] = TokenRow{Hash: "h-member", GitHubUserID: 1, GitHubLogin: "stays"}
	store.rows["h-gone"] = TokenRow{Hash: "h-gone", GitHubUserID: 2, GitHubLogin: "left"}
	v := &memberByLogin{members: map[string]bool{"stays": true}}
	s := &Sweeper{Store: store, Validate: v, Log: slog.New(slog.DiscardHandler)}

	if revoked := s.Sweep(context.Background()); revoked != 1 {
		t.Fatalf("revoked = %d, want 1", revoked)
	}
	if !store.has("h-member") {
		t.Error("member's token revoked")
	}
	if store.has("h-gone") {
		t.Error("departed member's token survived")
	}
}

func TestSweepErrorIsSkipNeverRevoke(t *testing.T) {
	store := newFakeStore()
	store.rows["h"] = TokenRow{Hash: "h", GitHubUserID: 1, GitHubLogin: "flaky"}
	v := &fakeValidator{err: errors.New("rate limited")}
	s := &Sweeper{Store: store, Validate: v, Log: slog.New(slog.DiscardHandler)}

	if revoked := s.Sweep(context.Background()); revoked != 0 {
		t.Fatalf("revoked = %d on API error", revoked)
	}
	if !store.has("h") {
		t.Fatal("token revoked on an API error — soft degradation violated")
	}
}

func TestSweepListFailureAborts(t *testing.T) {
	store := newFakeStore()
	store.listErr = errors.New("scan down")
	s := &Sweeper{Store: store, Validate: &fakeValidator{member: true}, Log: slog.New(slog.DiscardHandler)}
	if revoked := s.Sweep(context.Background()); revoked != 0 {
		t.Fatalf("revoked = %d after list failure", revoked)
	}
}

func TestSweepDeleteFailureContinues(t *testing.T) {
	store := newFakeStore()
	store.rows["h"] = TokenRow{Hash: "h", GitHubUserID: 1, GitHubLogin: "left"}
	store.writeErr = errors.New("dynamo down")
	s := &Sweeper{Store: store, Validate: &fakeValidator{member: false}, Log: slog.New(slog.DiscardHandler)}
	if revoked := s.Sweep(context.Background()); revoked != 0 {
		t.Fatalf("revoked = %d despite delete failure", revoked)
	}
}

func TestSweepRunStopsOnCancel(t *testing.T) {
	s := &Sweeper{
		Store:    newFakeStore(),
		Validate: &fakeValidator{member: true},
		Interval: time.Millisecond,
		Log:      slog.New(slog.DiscardHandler),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

// memberByLogin answers membership from a fixed set.
type memberByLogin struct {
	members map[string]bool
}

func (m *memberByLogin) Member(_ context.Context, _ int64, login string) (bool, error) {
	return m.members[login], nil
}
