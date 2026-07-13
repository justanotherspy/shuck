package gateway

import (
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/promexpo"
)

func TestMetricsSnapshot(t *testing.T) {
	var m Metrics
	m.ConnectionsLive.Store(3)
	m.DeliverRequests.Store(10)
	byName := map[string]promexpo.Sample{}
	for _, s := range m.Snapshot() {
		if !strings.HasPrefix(s.Name, "shuck_gateway_") {
			t.Errorf("name %q not namespaced", s.Name)
		}
		if s.Type == promexpo.Counter && !strings.HasSuffix(s.Name, "_total") &&
			!strings.HasSuffix(s.Name, "_sum") && !strings.HasSuffix(s.Name, "_count") {
			t.Errorf("counter %q missing _total/_sum/_count suffix", s.Name)
		}
		byName[s.Name] = s
	}
	if byName["shuck_gateway_connections_live"].Type != promexpo.Gauge {
		t.Errorf("connections_live should be a gauge")
	}
	if byName["shuck_gateway_connections_live"].Value != 3 {
		t.Errorf("connections_live = %d, want 3", byName["shuck_gateway_connections_live"].Value)
	}
	if byName["shuck_gateway_deliver_requests_total"].Value != 10 {
		t.Errorf("deliver_requests = %d, want 10", byName["shuck_gateway_deliver_requests_total"].Value)
	}
}

func TestNilMetricsSnapshot(t *testing.T) {
	var m *Metrics
	if s := m.Snapshot(); s != nil {
		t.Fatalf("nil snapshot = %v", s)
	}
}
