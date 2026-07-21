package worker

import (
	"sync/atomic"

	"github.com/justanotherspy/shuck/internal/promexpo"
)

// Metrics counts worker activity. All fields are safe for concurrent use.
// Counters are in-process only (reset on restart); cmd/shuck-worker logs a
// periodic snapshot in poll mode.
type Metrics struct {
	Received atomic.Int64 // envelopes that reached Process
	Invalid  atomic.Int64 // unparseable/unprocessable envelopes (poison)
	PRClosed atomic.Int64 // pr_closed pass-throughs delivered

	ReviewComments atomic.Int64 // review_comment events delivered
	Reviews        atomic.Int64 // review events delivered
	BotDropped     atomic.Int64 // review events dropped by the bot guard
	DupSkipped     atomic.Int64 // review events skipped by the standalone rule
	ReviewGone     atomic.Int64 // review objects deleted before the fetch

	TokenMints     atomic.Int64 // installation tokens minted from GitHub
	TokenCacheHits atomic.Int64 // token requests served from cache
	TokenErrors    atomic.Int64 // token mints that failed

	FetchErrors atomic.Int64 // run fetches that failed
	ParseErrors atomic.Int64 // distillations that failed (config bug)
	// FetchLatencySumMS / FetchLatencyCount track run-fetch latency per
	// envelope; average is sum/count. ParseLatency* likewise per job.
	FetchLatencySumMS atomic.Int64
	FetchLatencyCount atomic.Int64
	ParseLatencySumMS atomic.Int64
	ParseLatencyCount atomic.Int64

	Truncated        atomic.Int64 // summaries capped by CapSummary
	LogsArchived     atomic.Int64 // raw logs stored to the LogStore
	LogArchiveErrors atomic.Int64 // raw-log stores that failed (non-fatal)

	Delivered      atomic.Int64 // deliver calls accepted by the gateway
	DeliverRetries atomic.Int64 // deliver attempts retried (5xx/network)
	DeliverErrors  atomic.Int64 // deliver calls that failed terminally

	// RateRemaining is a gauge: the shared GitHub REST quota remaining at
	// the last fetch. All installations share one App quota, so a sinking
	// value here warns before every user is throttled at once.
	RateRemaining atomic.Int64
}

// Snapshot renders the current counters as Prometheus samples for the opt-in
// /metrics endpoint. Names mirror the fields cmd/shuck-worker logs,
// namespaced under shuck_worker_ and suffixed _total for counters. A nil
// receiver returns nil, so callers need not guard.
func (m *Metrics) Snapshot() []promexpo.Sample {
	if m == nil {
		return nil
	}
	c := func(name, help string, v int64) promexpo.Sample {
		return promexpo.Sample{Name: name, Help: help, Type: promexpo.Counter, Value: v}
	}
	g := func(name, help string, v int64) promexpo.Sample {
		return promexpo.Sample{Name: name, Help: help, Type: promexpo.Gauge, Value: v}
	}
	return []promexpo.Sample{
		c("shuck_worker_received_total", "Envelopes that reached Process.", m.Received.Load()),
		c("shuck_worker_invalid_total", "Unparseable or unprocessable envelopes (poison).", m.Invalid.Load()),
		c("shuck_worker_pr_closed_total", "pr_closed pass-throughs delivered.", m.PRClosed.Load()),
		c("shuck_worker_review_comments_total", "review_comment events delivered.", m.ReviewComments.Load()),
		c("shuck_worker_reviews_total", "review events delivered.", m.Reviews.Load()),
		c("shuck_worker_bot_dropped_total", "review events dropped by the bot guard.", m.BotDropped.Load()),
		c("shuck_worker_dup_skipped_total", "review events skipped by the standalone rule.", m.DupSkipped.Load()),
		c("shuck_worker_review_gone_total", "review objects deleted before the fetch.", m.ReviewGone.Load()),
		c("shuck_worker_token_mints_total", "Installation tokens minted from GitHub.", m.TokenMints.Load()),
		c("shuck_worker_token_cache_hits_total", "Token requests served from cache.", m.TokenCacheHits.Load()),
		c("shuck_worker_token_errors_total", "Token mints that failed.", m.TokenErrors.Load()),
		c("shuck_worker_fetch_errors_total", "Run fetches that failed.", m.FetchErrors.Load()),
		c("shuck_worker_parse_errors_total", "Distillations that failed.", m.ParseErrors.Load()),
		c("shuck_worker_fetch_latency_ms_sum", "Sum of run-fetch latency, milliseconds.", m.FetchLatencySumMS.Load()),
		c("shuck_worker_fetch_latency_ms_count", "Count of run-fetch latency observations.", m.FetchLatencyCount.Load()),
		c("shuck_worker_parse_latency_ms_sum", "Sum of per-job distillation latency, milliseconds.", m.ParseLatencySumMS.Load()),
		c("shuck_worker_parse_latency_ms_count", "Count of distillation latency observations.", m.ParseLatencyCount.Load()),
		c("shuck_worker_truncated_total", "Summaries capped by CapSummary.", m.Truncated.Load()),
		c("shuck_worker_logs_archived_total", "Raw logs stored to the LogStore.", m.LogsArchived.Load()),
		c("shuck_worker_log_archive_errors_total", "Raw-log stores that failed (non-fatal).", m.LogArchiveErrors.Load()),
		c("shuck_worker_delivered_total", "Deliver calls accepted by the gateway.", m.Delivered.Load()),
		c("shuck_worker_deliver_retries_total", "Deliver attempts retried (5xx or network).", m.DeliverRetries.Load()),
		c("shuck_worker_deliver_errors_total", "Deliver calls that failed terminally.", m.DeliverErrors.Load()),
		g("shuck_worker_rate_remaining", "Shared GitHub REST quota remaining at the last fetch.", m.RateRemaining.Load()),
	}
}
