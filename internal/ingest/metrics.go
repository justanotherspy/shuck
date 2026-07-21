package ingest

import "github.com/justanotherspy/shuck/internal/promexpo"

// Snapshot renders the current counters as Prometheus samples for the opt-in
// /metrics endpoint on cmd/shuck-ingest (server mode). Names mirror the
// counter fields, namespaced under shuck_ingest_ and suffixed _total. A nil
// receiver returns nil, so callers need not guard.
func (m *Metrics) Snapshot() []promexpo.Sample {
	if m == nil {
		return nil
	}
	c := func(name, help string, v int64) promexpo.Sample {
		return promexpo.Sample{Name: name, Help: help, Type: promexpo.Counter, Value: v}
	}
	return []promexpo.Sample{
		c("shuck_ingest_received_total", "Deliveries that reached the handler.", m.Received.Load()),
		c("shuck_ingest_verified_total", "Deliveries that passed signature verification.", m.Verified.Load()),
		c("shuck_ingest_deduped_total", "Deliveries dropped as redeliveries.", m.Deduped.Load()),
		c("shuck_ingest_dropped_total", "Deliveries filtered out by event/action/conclusion.", m.Dropped.Load()),
		c("shuck_ingest_unsubscribed_total", "Deliveries dropped by the subscription pre-filter.", m.Unsubscribed.Load()),
		c("shuck_ingest_enqueued_total", "Envelopes handed to the queue.", m.Enqueued.Load()),
		c("shuck_ingest_errors_total", "Dedupe, enqueue, or payload failures.", m.Errors.Load()),
	}
}
