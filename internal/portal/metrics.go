package portal

import (
	"sync/atomic"

	"github.com/justanotherspy/shuck/internal/promexpo"
)

// Metrics counts portal activity for the opt-in /metrics endpoint. All fields
// are safe for concurrent use and complement the per-event audit slog lines
// (which remain the record of who did what — these are just aggregates for
// alerting). Counters are in-process only and reset on restart; a Lambda
// portal has no resident process to scrape, so these back the resident
// (Helm) deployment. Every mutator is nil-safe so wiring Metrics is optional.
type Metrics struct {
	TokensMinted      atomic.Int64 // first-time token mints
	TokensRegenerated atomic.Int64 // mints that revoked a prior token
	MintErrors        atomic.Int64 // Mint calls that failed
	MembershipDenied  atomic.Int64 // mint refused: definitive non-member
	MembershipUnknown atomic.Int64 // mint blocked: membership check errored
	SweepPasses       atomic.Int64 // grace-window / re-validation passes run
	SweepRevoked      atomic.Int64 // tokens revoked by a sweep (departed members)
	SweepUnknown      atomic.Int64 // sweep membership checks that errored (skipped, never revoked)
}

func (m *Metrics) incMinted() {
	if m != nil {
		m.TokensMinted.Add(1)
	}
}

func (m *Metrics) incRegenerated() {
	if m != nil {
		m.TokensRegenerated.Add(1)
	}
}

func (m *Metrics) incMintErrors() {
	if m != nil {
		m.MintErrors.Add(1)
	}
}

func (m *Metrics) incMembershipDenied() {
	if m != nil {
		m.MembershipDenied.Add(1)
	}
}

func (m *Metrics) incMembershipUnknown() {
	if m != nil {
		m.MembershipUnknown.Add(1)
	}
}

func (m *Metrics) incSweepPass() {
	if m != nil {
		m.SweepPasses.Add(1)
	}
}

func (m *Metrics) addSweep(revoked, unknown int64) {
	if m != nil {
		m.SweepRevoked.Add(revoked)
		m.SweepUnknown.Add(unknown)
	}
}

// Snapshot renders the current counters as Prometheus samples. Names are
// namespaced under shuck_portal_ and suffixed _total. A nil receiver returns
// nil, so callers need not guard.
func (m *Metrics) Snapshot() []promexpo.Sample {
	if m == nil {
		return nil
	}
	c := func(name, help string, v int64) promexpo.Sample {
		return promexpo.Sample{Name: name, Help: help, Type: promexpo.Counter, Value: v}
	}
	return []promexpo.Sample{
		c("shuck_portal_tokens_minted_total", "First-time token mints.", m.TokensMinted.Load()),
		c("shuck_portal_tokens_regenerated_total", "Mints that revoked a prior token.", m.TokensRegenerated.Load()),
		c("shuck_portal_mint_errors_total", "Mint calls that failed.", m.MintErrors.Load()),
		c("shuck_portal_membership_denied_total", "Mints refused: definitive non-member.", m.MembershipDenied.Load()),
		c("shuck_portal_membership_unknown_total", "Mints blocked: membership check errored.", m.MembershipUnknown.Load()),
		c("shuck_portal_sweep_passes_total", "Re-validation sweep passes run.", m.SweepPasses.Load()),
		c("shuck_portal_sweep_revoked_total", "Tokens revoked by a sweep (departed members).", m.SweepRevoked.Load()),
		c("shuck_portal_sweep_unknown_total", "Sweep membership checks that errored (skipped, never revoked).", m.SweepUnknown.Load()),
	}
}
