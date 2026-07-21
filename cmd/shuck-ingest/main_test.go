package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/ingest"
)

func TestParseTTL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"valid", "2h", 2 * time.Hour, false},
		{"zero silently disables dedupe", "0s", 0, true},
		{"negative silently disables dedupe", "-1h", 0, true},
		{"unparseable", "bogus", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTTL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseTTL(%q) = %s, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTTL(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("parseTTL(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewServerTimeouts(t *testing.T) {
	// The webhook endpoint is public: ReadHeaderTimeout alone leaves a
	// body-slowloris holding connections forever, so the whole read and
	// idle keep-alives must be bounded too.
	s := newServer(":0", http.NewServeMux())
	if s.ReadHeaderTimeout <= 0 {
		t.Error("ReadHeaderTimeout unset")
	}
	if s.ReadTimeout <= 0 {
		t.Error("ReadTimeout unset: a slow body would hold the connection forever")
	}
	if s.IdleTimeout <= 0 {
		t.Error("IdleTimeout unset: idle keep-alives would pile up")
	}
}

// syncBuffer is a bytes.Buffer safe for the logMetrics goroutine to write
// while the test polls it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestLogMetricsSnapshot(t *testing.T) {
	buf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(buf, nil))
	m := &ingest.Metrics{}
	m.Received.Add(3)
	m.Verified.Add(3)
	m.Enqueued.Add(2)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		logMetrics(ctx, log, m, time.Millisecond)
	}()
	deadline := time.After(5 * time.Second)
	for !strings.Contains(buf.String(), "msg=metrics") {
		select {
		case <-deadline:
			t.Fatal("no metrics snapshot logged")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done // the ticker must stop on shutdown

	out := buf.String()
	for _, want := range []string{
		"received=3", "verified=3", "deduped=0", "dropped=0",
		"unsubscribed=0", "enqueued=2", "errors=0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot missing %q in %q", want, out)
		}
	}
}
