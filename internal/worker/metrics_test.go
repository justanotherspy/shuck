package worker

import (
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/promexpo"
)

func TestMetricsSnapshot(t *testing.T) {
	var m Metrics
	m.Delivered.Store(42)
	m.RateRemaining.Store(4999)
	byName := map[string]promexpo.Sample{}
	for _, s := range m.Snapshot() {
		if !strings.HasPrefix(s.Name, "shuck_worker_") {
			t.Errorf("name %q not namespaced", s.Name)
		}
		byName[s.Name] = s
	}
	if byName["shuck_worker_delivered_total"].Value != 42 {
		t.Errorf("delivered = %d, want 42", byName["shuck_worker_delivered_total"].Value)
	}
	if byName["shuck_worker_rate_remaining"].Type != promexpo.Gauge {
		t.Errorf("rate_remaining should be a gauge")
	}
	if byName["shuck_worker_rate_remaining"].Value != 4999 {
		t.Errorf("rate_remaining = %d, want 4999", byName["shuck_worker_rate_remaining"].Value)
	}
}

func TestNilMetricsSnapshot(t *testing.T) {
	var m *Metrics
	if s := m.Snapshot(); s != nil {
		t.Fatalf("nil snapshot = %v", s)
	}
}
