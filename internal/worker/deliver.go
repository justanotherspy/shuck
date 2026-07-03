package worker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/justanotherspy/shuck/internal/gateway"
)

// Deliver retry defaults: three attempts with a doubling backoff bounds a
// gateway deploy blip without stretching the queue visibility timeout.
const (
	DefaultDeliverAttempts = 3
	DefaultDeliverBackoff  = 500 * time.Millisecond
)

// HTTPDeliverer POSTs deliver requests to the gateway's /internal/deliver
// endpoint, authenticated with the shared deliver secret on every call. 5xx
// and transport errors are retried with backoff — the gateway dedupes on
// event_id, so overlap is harmless; a 4xx is a worker-side contract bug and
// is returned immediately (retrying the same payload cannot help). It
// implements Deliverer with only the standard library.
type HTTPDeliverer struct {
	// URL is the full deliver endpoint URL.
	URL string
	// Secret is sent as the gateway's deliver-secret header on every call.
	Secret string
	// HTTP may be nil, which means a 30-second-timeout client.
	HTTP *http.Client
	// MaxAttempts caps tries per Deliver call; 0 means DefaultDeliverAttempts.
	MaxAttempts int
	// Backoff is the first retry's delay, doubled per retry; 0 means
	// DefaultDeliverBackoff.
	Backoff time.Duration
	// Sleep may be nil, which means a context-aware real sleep; a no-op in
	// tests.
	Sleep func(ctx context.Context, d time.Duration) error
	// Log may be nil, which means slog.Default().
	Log *slog.Logger
	// Metrics may be nil, which disables counting.
	Metrics *Metrics
}

// Deliver encodes and POSTs one request, retrying transient failures.
func (d *HTTPDeliverer) Deliver(ctx context.Context, req gateway.DeliverRequest) error {
	body, err := req.Encode()
	if err != nil {
		return fmt.Errorf("encode deliver request: %w", err)
	}

	attempts := d.MaxAttempts
	if attempts <= 0 {
		attempts = DefaultDeliverAttempts
	}
	backoff := d.Backoff
	if backoff <= 0 {
		backoff = DefaultDeliverBackoff
	}

	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			d.count(func(m *Metrics) { m.DeliverRetries.Add(1) })
			d.log().Warn("retrying deliver", "event_id", req.EventID, "attempt", attempt+1, "err", lastErr)
			if err := d.sleep(ctx, backoff<<(attempt-1)); err != nil {
				return err
			}
		}

		retryable, err := d.post(ctx, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable {
			return fmt.Errorf("deliver %s: %w", req.EventID, err)
		}
	}
	return fmt.Errorf("deliver %s after %d attempts: %w", req.EventID, attempts, lastErr)
}

// post sends one attempt; retryable reports whether a failure is transient.
func (d *HTTPDeliverer) post(ctx context.Context, body []byte) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(gateway.DeliverSecretHeader, d.Secret)

	resp, err := d.http().Do(req)
	if err != nil {
		return true, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain so the connection is reusable; the body carries no contract.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode >= 500:
		return true, fmt.Errorf("gateway returned %s", resp.Status)
	default:
		return false, fmt.Errorf("gateway rejected deliver: %s", resp.Status)
	}
}

func (d *HTTPDeliverer) sleep(ctx context.Context, dur time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, dur)
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (d *HTTPDeliverer) http() *http.Client {
	if d.HTTP == nil {
		return &http.Client{Timeout: 30 * time.Second}
	}
	return d.HTTP
}

func (d *HTTPDeliverer) log() *slog.Logger {
	if d.Log == nil {
		return slog.Default()
	}
	return d.Log
}

func (d *HTTPDeliverer) count(f func(*Metrics)) {
	if d.Metrics != nil {
		f(d.Metrics)
	}
}
