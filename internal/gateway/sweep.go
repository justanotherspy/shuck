package gateway

import (
	"context"
	"log/slog"
	"time"
)

// Default sweep tuning.
const (
	// DefaultGraceWindow is how long a disconnected subscriber keeps its
	// subscriptions and buffered events before the sweep removes them.
	DefaultGraceWindow = 24 * time.Hour
	// DefaultSweepInterval is how often the sweep runs.
	DefaultSweepInterval = 15 * time.Minute
)

// Sweeper removes the subscriptions, buffered events, and presence of
// subscribers that have been disconnected for longer than the grace window.
type Sweeper struct {
	Subs     SubscriptionStore
	Buffer   EventBuffer
	Presence PresenceStore
	// Registry guards live connections from being swept: a subscriber
	// with a registered connection is never stale, whatever its presence
	// row says (e.g. after a crash left rows looking connected).
	Registry Registry
	// Live optionally consults a durable connection registry — the
	// serverless shape, where each sweep runs in a fresh process and the
	// in-memory Registry is always empty. A subscriber with a live
	// registry row is never swept; a lookup error errs on the safe side
	// (skip this pass, retry next time). Nil means Registry alone decides.
	Live func(ctx context.Context, sub SubscriberKey) (bool, error)
	// GraceWindow and Interval fall back to their Default* constants when
	// zero.
	GraceWindow time.Duration
	Interval    time.Duration
	// Log may be nil, which means slog.Default().
	Log *slog.Logger
	// Metrics may be nil, which disables counting.
	Metrics *Metrics
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

// Sweep runs one pass: list stale subscribers, skip any that are live, and
// remove the rest's subscriptions, buffer, and presence. Failures are
// logged and retried naturally on the next pass.
func (s *Sweeper) Sweep(ctx context.Context) {
	grace := s.GraceWindow
	if grace <= 0 {
		grace = DefaultGraceWindow
	}
	cutoff := s.clock()().Add(-grace)
	stale, err := s.Presence.Stale(ctx, cutoff)
	if err != nil {
		s.log().Error("sweep: stale listing failed", "err", err)
		return
	}
	for _, sub := range stale {
		if _, live := s.Registry.Get(sub); live {
			continue
		}
		if s.Live != nil {
			live, err := s.Live(ctx, sub)
			if err != nil {
				s.log().Warn("sweep: durable registry check failed; skipping subscriber", "subscriber", sub.String(), "err", err)
				continue
			}
			if live {
				continue
			}
		}
		if err := s.Subs.RemoveAllForSubscriber(ctx, sub); err != nil {
			s.log().Error("sweep: remove subscriptions failed", "subscriber", sub.String(), "err", err)
			continue
		}
		if err := s.Buffer.Purge(ctx, sub); err != nil {
			s.log().Error("sweep: purge buffer failed", "subscriber", sub.String(), "err", err)
			continue
		}
		if err := s.Presence.Delete(ctx, sub); err != nil {
			s.log().Error("sweep: delete presence failed", "subscriber", sub.String(), "err", err)
			continue
		}
		if s.Metrics != nil {
			s.Metrics.SweepRemoved.Add(1)
		}
		s.log().Info("sweep: removed stale subscriber", "subscriber", sub.String())
	}
}

func (s *Sweeper) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Sweeper) clock() func() time.Time {
	if s.Now != nil {
		return s.Now
	}
	return time.Now
}
