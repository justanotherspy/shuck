package worker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/gateway"
)

func deliverReq() gateway.DeliverRequest {
	return gateway.DeliverRequest{
		Schema:  gateway.DeliverSchema,
		EventID: "delivery-1",
		Kind:    gateway.KindCIFailure,
		Repo:    "o/r",
		PR:      7,
		Summary: "ci: failure — 1 failed step(s)",
	}
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestHTTPDelivererAccepted(t *testing.T) {
	var got gateway.DeliverRequest
	var gotSecret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSecret = r.Header.Get(gateway.DeliverSecretHeader)
		body, _ := io.ReadAll(r.Body)
		var err error
		if got, err = gateway.ParseDeliverRequest(body); err != nil {
			t.Errorf("body does not parse as a deliver request: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	d := &HTTPDeliverer{URL: srv.URL, Secret: "s3cret", HTTP: srv.Client(), Sleep: noSleep}
	if err := d.Deliver(context.Background(), deliverReq()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if gotSecret != "s3cret" {
		t.Errorf("secret header = %q", gotSecret)
	}
	if got != deliverReq() {
		t.Errorf("gateway received %+v", got)
	}
}

func TestHTTPDelivererRetriesServerErrors(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "buffering failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	metrics := &Metrics{}
	d := &HTTPDeliverer{URL: srv.URL, HTTP: srv.Client(), Sleep: noSleep, Metrics: metrics}
	if err := d.Deliver(context.Background(), deliverReq()); err != nil {
		t.Fatalf("Deliver should succeed on the third attempt: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("gateway called %d times, want 3", calls.Load())
	}
	if metrics.DeliverRetries.Load() != 2 {
		t.Errorf("retries counted = %d, want 2", metrics.DeliverRetries.Load())
	}
}

func TestHTTPDelivererExhaustsRetries(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	d := &HTTPDeliverer{URL: srv.URL, HTTP: srv.Client(), MaxAttempts: 2, Sleep: noSleep}
	if err := d.Deliver(context.Background(), deliverReq()); err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if calls.Load() != 2 {
		t.Errorf("gateway called %d times, want 2", calls.Load())
	}
}

func TestHTTPDelivererClientErrorIsPermanent(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad schema", http.StatusBadRequest)
	}))
	defer srv.Close()

	d := &HTTPDeliverer{URL: srv.URL, HTTP: srv.Client(), Sleep: noSleep}
	if err := d.Deliver(context.Background(), deliverReq()); err == nil {
		t.Fatal("want error on 400")
	}
	if calls.Load() != 1 {
		t.Errorf("a 4xx must not be retried; gateway called %d times", calls.Load())
	}
}

func TestHTTPDelivererRetriesTransportErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // connection refused from here on

	var slept atomic.Int64
	d := &HTTPDeliverer{URL: srv.URL, MaxAttempts: 2,
		Sleep: func(context.Context, time.Duration) error { slept.Add(1); return nil }}
	if err := d.Deliver(context.Background(), deliverReq()); err == nil {
		t.Fatal("want error when the gateway is unreachable")
	}
	if slept.Load() != 1 {
		t.Errorf("slept %d times, want 1 (between 2 attempts)", slept.Load())
	}
}

func TestHTTPDelivererSleepHonoursContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &HTTPDeliverer{URL: srv.URL, HTTP: srv.Client(), Backoff: time.Hour}
	start := time.Now()
	if err := d.Deliver(ctx, deliverReq()); err == nil {
		t.Fatal("want context error")
	}
	if time.Since(start) > time.Second {
		t.Fatal("real sleep ignored the cancelled context")
	}
}
