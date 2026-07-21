package portal

import (
	"context"
	"testing"
)

func sampleValue(t *testing.T, m *Metrics, name string) int64 {
	t.Helper()
	for _, s := range m.Snapshot() {
		if s.Name == name {
			return s.Value
		}
	}
	t.Fatalf("sample %q not found in snapshot", name)
	return 0
}

func TestMetricsSnapshotNames(t *testing.T) {
	var m Metrics
	m.TokensMinted.Store(2)
	m.TokensRegenerated.Store(1)
	got := map[string]int64{}
	for _, s := range m.Snapshot() {
		got[s.Name] = s.Value
	}
	for _, name := range []string{
		"shuck_portal_tokens_minted_total",
		"shuck_portal_tokens_regenerated_total",
		"shuck_portal_mint_errors_total",
		"shuck_portal_membership_denied_total",
		"shuck_portal_membership_unknown_total",
		"shuck_portal_sweep_passes_total",
		"shuck_portal_sweep_revoked_total",
		"shuck_portal_sweep_unknown_total",
	} {
		if _, ok := got[name]; !ok {
			t.Errorf("missing sample %q", name)
		}
	}
	if got["shuck_portal_tokens_minted_total"] != 2 || got["shuck_portal_tokens_regenerated_total"] != 1 {
		t.Errorf("unexpected values: %v", got)
	}
}

func TestNilMetricsSnapshot(t *testing.T) {
	var m *Metrics
	if s := m.Snapshot(); s != nil {
		t.Fatalf("nil snapshot = %v", s)
	}
	// Mutators must be nil-safe.
	m.incMinted()
	m.incRegenerated()
	m.incMintErrors()
	m.incMembershipDenied()
	m.incMembershipUnknown()
	m.incSweepPass()
	m.addSweep(1, 1)
}

// perUserValidator answers membership from a per-user-id map; a userID in
// errIDs returns an error (an "unknown" skip).
type perUserValidator struct {
	member map[int64]bool
	errIDs map[int64]bool
}

func (v perUserValidator) Member(_ context.Context, userID int64, _ string) (bool, error) {
	if v.errIDs[userID] {
		return false, context.DeadlineExceeded
	}
	return v.member[userID], nil
}

func TestSweepMetrics(t *testing.T) {
	store := newFakeStore()
	store.rows["h1"] = TokenRow{Hash: "h1", GitHubUserID: 1, GitHubLogin: "a"} // member: keep
	store.rows["h2"] = TokenRow{Hash: "h2", GitHubUserID: 2, GitHubLogin: "b"} // non-member: revoke
	store.rows["h3"] = TokenRow{Hash: "h3", GitHubUserID: 3, GitHubLogin: "c"} // error: unknown/skip
	m := &Metrics{}
	s := &Sweeper{
		Store: store,
		Validate: perUserValidator{
			member: map[int64]bool{1: true, 2: false},
			errIDs: map[int64]bool{3: true},
		},
		Metrics: m,
	}
	revoked := s.Sweep(context.Background())
	if revoked != 1 {
		t.Fatalf("revoked = %d, want 1", revoked)
	}
	if v := sampleValue(t, m, "shuck_portal_sweep_revoked_total"); v != 1 {
		t.Errorf("sweep_revoked = %d, want 1", v)
	}
	if v := sampleValue(t, m, "shuck_portal_sweep_unknown_total"); v != 1 {
		t.Errorf("sweep_unknown = %d, want 1", v)
	}
	if v := sampleValue(t, m, "shuck_portal_sweep_passes_total"); v != 1 {
		t.Errorf("sweep_passes = %d, want 1", v)
	}
	// h3 (error) must survive — soft degradation, never a false revoke.
	if !store.has("h3") {
		t.Errorf("errored row was revoked; sweep must skip unknowns")
	}
}
