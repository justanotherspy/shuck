package worker

import "sync/atomic"

// Metrics counts worker activity. All fields are safe for concurrent use.
// Counters are in-process only (reset on restart); cmd/shuck-worker logs a
// periodic snapshot in poll mode.
type Metrics struct {
	Received atomic.Int64 // envelopes that reached Process
	Invalid  atomic.Int64 // unparseable/unprocessable envelopes (poison)
	PRClosed atomic.Int64 // pr_closed pass-throughs delivered

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
