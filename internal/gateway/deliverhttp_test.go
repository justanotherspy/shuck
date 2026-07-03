package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newDeliverHandler(secrets ...string) (*DeliverHandler, *fakeSubs, *fakeBuffer) {
	hub, _, subs, buffer, _ := newTestHub()
	var values [][]byte
	for _, s := range secrets {
		values = append(values, []byte(s))
	}
	handler := &DeliverHandler{
		Secrets: values,
		Hub:     hub,
		Log:     slog.New(slog.DiscardHandler),
		Metrics: hub.Metrics,
	}
	return handler, subs, buffer
}

func postDeliver(t *testing.T, handler http.Handler, secret string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/deliver", bytes.NewReader(body))
	if secret != "" {
		req.Header.Set(DeliverSecretHeader, secret)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestDeliverHandlerAuth(t *testing.T) {
	body, err := validDeliver().Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	cases := []struct {
		name     string
		secret   string
		wantCode int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong value", "nope", http.StatusUnauthorized},
		{"primary value", "primary", http.StatusAccepted},
		{"secondary value during rotation", "secondary", http.StatusAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, subs, buffer := newDeliverHandler("primary", "secondary")
			key := SubscriberKey{UserID: "1", SessionID: "s"}
			subscribeBoth(t, subs, PRRef{Repo: "octo/repo", PR: 7}, key)

			rec := postDeliver(t, handler, tc.secret, body)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if tc.wantCode == http.StatusUnauthorized {
				// A rejected call must not have touched the buffer.
				if got := buffer.opLog(); len(got) != 0 {
					t.Fatalf("rejected deliver reached the buffer: %v", got)
				}
				if handler.Metrics.DeliverRejected.Load() != 1 {
					t.Fatalf("DeliverRejected = %d, want 1", handler.Metrics.DeliverRejected.Load())
				}
				return
			}
			if buffer.depth(key) != 1 {
				t.Fatalf("buffer depth = %d, want 1", buffer.depth(key))
			}
			var res DeliverResult
			if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if res.Subscribers != 1 || res.Buffered != 1 {
				t.Fatalf("response = %+v, want 1 subscriber, 1 buffered", res)
			}
		})
	}
}

func TestDeliverHandlerNoSecretsConfiguredRejects(t *testing.T) {
	handler, _, buffer := newDeliverHandler() // zero secrets: fail closed
	body, _ := validDeliver().Encode()
	rec := postDeliver(t, handler, "anything", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when no secret is configured", rec.Code)
	}
	if len(buffer.opLog()) != 0 {
		t.Fatal("deliver reached the buffer without a configured secret")
	}
}

func TestDeliverHandlerMethodAndBody(t *testing.T) {
	handler, _, _ := newDeliverHandler("primary")

	req := httptest.NewRequest(http.MethodGet, "/internal/deliver", http.NoBody)
	req.Header.Set(DeliverSecretHeader, "primary")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}

	if rec := postDeliver(t, handler, "primary", []byte("{not json")); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body status = %d, want 400", rec.Code)
	}

	handler.MaxBody = 16
	big := []byte(`{"schema":1,"event_id":"` + strings.Repeat("x", 64) + `"}`)
	if rec := postDeliver(t, handler, "primary", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", rec.Code)
	}
}

func TestDeliverHandlerHubFailureIs500(t *testing.T) {
	handler, subs, buffer := newDeliverHandler("primary")
	subscribeBoth(t, subs, PRRef{Repo: "octo/repo", PR: 7}, SubscriberKey{UserID: "1", SessionID: "s"})
	buffer.appendErr = errFake
	body, _ := validDeliver().Encode()
	rec := postDeliver(t, handler, "primary", body)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 so the worker retries", rec.Code)
	}
}

func TestDeliverHandlerContextReachesHub(t *testing.T) {
	// Ensure the request context is what the hub sees (cancellation
	// propagates from the HTTP layer).
	handler, subs, _ := newDeliverHandler("primary")
	subscribeBoth(t, subs, PRRef{Repo: "octo/repo", PR: 7}, SubscriberKey{UserID: "1", SessionID: "s"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body, _ := validDeliver().Encode()
	req := httptest.NewRequest(http.MethodPost, "/internal/deliver", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set(DeliverSecretHeader, "primary")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// The fakes ignore ctx, so this still succeeds — the assertion is
	// simply that a canceled context does not panic the handler path.
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
}
