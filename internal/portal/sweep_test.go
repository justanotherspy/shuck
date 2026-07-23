package portal

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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

// TestSweepStaleLoginNeverFalseRevokes pins the rename-safety contract end
// to end: the sweep validates rows whose stored login went stale (the user
// renamed on GitHub) via the OrgValidator, which re-resolves the current
// login from the immutable user ID before probing.
func TestSweepStaleLoginNeverFalseRevokes(t *testing.T) {
	tests := []struct {
		name        string
		api         *fakeOrgAPI
		wantRevoked int
		wantKept    bool
	}{
		{
			name:        "renamed member keeps the token",
			api:         &fakeOrgAPI{member: true, loginByID: "new-name", idFound: true},
			wantRevoked: 0,
			wantKept:    true,
		},
		{
			name:        "deleted account is revoked",
			api:         &fakeOrgAPI{idFound: false},
			wantRevoked: 1,
		},
		{
			name:        "lookup failure is skipped, never revoked",
			api:         &fakeOrgAPI{idErr: errors.New("500 from github")},
			wantRevoked: 0,
			wantKept:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			store.rows["h"] = TokenRow{Hash: "h", GitHubUserID: 7, GitHubLogin: "old-name"}
			s := &Sweeper{Store: store, Validate: orgValidatorFor(tt.api), Log: slog.New(slog.DiscardHandler)}
			if revoked := s.Sweep(context.Background()); revoked != tt.wantRevoked {
				t.Fatalf("revoked = %d, want %d", revoked, tt.wantRevoked)
			}
			if store.has("h") != tt.wantKept {
				t.Fatalf("row kept = %v, want %v", store.has("h"), tt.wantKept)
			}
			if tt.api.gotID != 7 {
				t.Errorf("login resolved for user %d, want 7 (the row's immutable id)", tt.api.gotID)
			}
			if tt.wantKept && tt.wantRevoked == 0 && tt.api.idErr == nil && tt.api.gotLogin != "new-name" {
				t.Errorf("membership probed with %q, want the fresh login, never the stale one", tt.api.gotLogin)
			}
		})
	}
}

// TestSweepAccountValidator covers the personal-install mode: only rows not
// owned by the installation account are revoked.
func TestSweepAccountValidator(t *testing.T) {
	store := newFakeStore()
	store.rows["h-owner"] = TokenRow{Hash: "h-owner", GitHubUserID: 42, GitHubLogin: "owner"}
	store.rows["h-foreign"] = TokenRow{Hash: "h-foreign", GitHubUserID: 7, GitHubLogin: "intruder"}
	s := &Sweeper{Store: store, Validate: &AccountValidator{AccountID: 42}, Log: slog.New(slog.DiscardHandler)}

	if revoked := s.Sweep(context.Background()); revoked != 1 {
		t.Fatalf("revoked = %d, want 1", revoked)
	}
	if !store.has("h-owner") {
		t.Error("owner's token revoked")
	}
	if store.has("h-foreign") {
		t.Error("foreign row survived the consistency sweep")
	}
}

// signalStore flags every All call so tests can observe sweep passes across
// goroutines without racing on counters.
type signalStore struct {
	*fakeStore
	swept chan struct{}
}

func (s *signalStore) All(ctx context.Context) ([]TokenRow, error) {
	select {
	case s.swept <- struct{}{}:
	default:
	}
	return s.fakeStore.All(ctx)
}

func TestSweepRunSweepsImmediately(t *testing.T) {
	store := &signalStore{fakeStore: newFakeStore(), swept: make(chan struct{}, 1)}
	log := &capturingLog{}
	s := &Sweeper{
		Store:    store,
		Validate: &fakeValidator{member: true},
		Interval: time.Hour, // far beyond the test: only the initial pass can fire
		Log:      log.logger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	select {
	case <-store.swept:
	case <-time.After(5 * time.Second):
		t.Fatal("no sweep before the first tick — a restart-prone server would never sweep")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
	if !strings.Contains(log.String(), "sweep pass finished") {
		t.Error("server-mode pass result not logged")
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
