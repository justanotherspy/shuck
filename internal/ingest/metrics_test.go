package ingest

import (
	"strings"
	"testing"
)

func TestMetricsSnapshot(t *testing.T) {
	var m Metrics
	m.Received.Store(5)
	m.Enqueued.Store(2)
	got := map[string]int64{}
	for _, s := range m.Snapshot() {
		if !strings.HasPrefix(s.Name, "shuck_ingest_") {
			t.Errorf("name %q not namespaced", s.Name)
		}
		if !strings.HasSuffix(s.Name, "_total") {
			t.Errorf("counter %q missing _total suffix", s.Name)
		}
		got[s.Name] = s.Value
	}
	if got["shuck_ingest_received_total"] != 5 {
		t.Errorf("received = %d, want 5", got["shuck_ingest_received_total"])
	}
	if got["shuck_ingest_enqueued_total"] != 2 {
		t.Errorf("enqueued = %d, want 2", got["shuck_ingest_enqueued_total"])
	}
}

func TestNilMetricsSnapshot(t *testing.T) {
	var m *Metrics
	if s := m.Snapshot(); s != nil {
		t.Fatalf("nil snapshot = %v", s)
	}
}
