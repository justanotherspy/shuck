package portal

import (
	"context"
	"log/slog"
	"time"
)

// DefaultSweepInterval is how often the re-validation sweep runs. GitHub
// org membership is the access-control plane, so daily is the offboarding
// latency bound.
const DefaultSweepInterval = 24 * time.Hour

// Sweeper re-validates every token's GitHub user against the current org
// membership and revokes departed members. Personal-install deployments get
// a consistency check instead: any row not owned by the account is revoked.
type Sweeper struct {
	Store    TokenStore
	Validate Validator
	// Interval falls back to DefaultSweepInterval when zero.
	Interval time.Duration
	// Log may be nil, which means slog.Default().
	Log *slog.Logger
	// Now may be nil, which means time.Now.
	Now func() time.Time
}

// Run sweeps on the configured interval until ctx is done.
func (s *Sweeper) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Sweep(ctx)
		}
	}
}

// Sweep runs one pass. A validation error is a skipped check — the token
// survives until a pass gets a definitive answer (soft degradation, never a
// false revoke); only a definitive non-member loses the row. revoked
// reports how many tokens were removed, for the one-shot cmd mode's logs.
func (s *Sweeper) Sweep(ctx context.Context) (revoked int) {
	rows, err := s.Store.All(ctx)
	if err != nil {
		s.log().Error("sweep: token listing failed", "err", err)
		return 0
	}
	for _, row := range rows {
		member, err := s.Validate.Member(ctx, row.GitHubUserID, row.GitHubLogin)
		if err != nil {
			s.log().Warn("sweep: membership check failed, skipping",
				"github_user_id", row.GitHubUserID, "github_login", row.GitHubLogin, "err", err)
			continue
		}
		if member {
			continue
		}
		if err := s.Store.Delete(ctx, row.Hash); err != nil {
			s.log().Error("sweep: revoke failed",
				"github_user_id", row.GitHubUserID, "github_login", row.GitHubLogin, "err", err)
			continue
		}
		revoked++
		s.log().Info("audit: token revoked by sweep", "event", "token_revoked_sweep",
			"github_user_id", row.GitHubUserID, "github_login", row.GitHubLogin,
			"token_hash", row.Hash)
	}
	return revoked
}

func (s *Sweeper) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}
