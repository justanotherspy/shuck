package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
)

// DefaultMaxBody caps the webhook request body when Handler.MaxBody is
// unset. GitHub caps payloads at 25 MB; the events shuck routes are a few
// tens of KB, so 10 MB leaves generous headroom while bounding memory.
const DefaultMaxBody = 10 << 20

// deliveryHeader carries the GitHub delivery GUID used for dedupe.
const deliveryHeader = "X-Github-Delivery"

// eventHeader names the webhook event type.
const eventHeader = "X-Github-Event"

// Deduper records webhook delivery GUIDs so GitHub redeliveries are
// processed once.
type Deduper interface {
	// Seen atomically records id and reports whether it was already
	// recorded (a conditional put in the DynamoDB implementation).
	Seen(ctx context.Context, id string) (bool, error)
	// Forget removes id. It is best-effort cleanup after a failed enqueue,
	// so a GitHub redelivery of the same GUID is not dropped as a duplicate.
	Forget(ctx context.Context, id string) error
}

// Enqueuer hands an envelope to the work queue consumed by JUS-87 workers.
type Enqueuer interface {
	Enqueue(ctx context.Context, env Envelope) error
}

// SubscriptionChecker is the cheap pre-filter: skip enqueueing when nobody
// is subscribed to repo#pr. It is an optimisation, never a gate — the
// handler fails open when it errors.
type SubscriptionChecker interface {
	HasSubscriber(ctx context.Context, repo string, pr int) (bool, error)
}

// AllowAll is the SubscriptionChecker used until the JUS-88 subscription
// table exists: every repo#pr is assumed subscribed, making the pre-filter
// a no-op that can be tightened without touching the handler.
type AllowAll struct{}

// HasSubscriber always reports true.
func (AllowAll) HasSubscriber(context.Context, string, int) (bool, error) {
	return true, nil
}

// Metrics counts delivery outcomes. All fields are safe for concurrent use.
type Metrics struct {
	Received     atomic.Int64 // deliveries that reached the handler
	Verified     atomic.Int64 // passed signature verification
	Deduped      atomic.Int64 // dropped as redeliveries
	Dropped      atomic.Int64 // filtered out (event/action/conclusion)
	Unsubscribed atomic.Int64 // dropped by the subscription pre-filter
	Enqueued     atomic.Int64 // envelopes handed to the queue
	Errors       atomic.Int64 // dedupe/enqueue/payload failures
}

// Handler is the webhook ingest http.Handler. The same instance backs both
// entrypoints of cmd/shuck-ingest (plain HTTP server and Lambda).
type Handler struct {
	// Secret is the GitHub App webhook secret; requests that do not carry a
	// valid X-Hub-Signature-256 over it are rejected before parsing.
	Secret []byte
	Dedupe Deduper
	Queue  Enqueuer
	// Subs may be nil, which means AllowAll.
	Subs SubscriptionChecker
	// Log may be nil, which means slog.Default().
	Log *slog.Logger
	// Metrics may be nil, which disables counting.
	Metrics *Metrics
	// MaxBody caps the request body size; 0 means DefaultMaxBody.
	MaxBody int64
}

// ServeHTTP runs one delivery through verify → dedupe → filter →
// subscription pre-filter → enqueue. Every drop is a 2xx so GitHub does not
// retry; only operational failures are 5xx.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.count(func(m *Metrics) { m.Received.Add(1) })

	maxBody := h.MaxBody
	if maxBody <= 0 {
		maxBody = DefaultMaxBody
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxBody {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !Verify(h.Secret, body, r.Header.Get(SignatureHeader)) {
		h.log().Warn("signature rejected", "delivery", r.Header.Get(deliveryHeader))
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	h.count(func(m *Metrics) { m.Verified.Add(1) })

	delivery := r.Header.Get(deliveryHeader)
	event := r.Header.Get(eventHeader)
	if delivery == "" || event == "" {
		http.Error(w, "missing delivery headers", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	seen, err := h.Dedupe.Seen(ctx, delivery)
	if err != nil {
		h.fail(w, "dedupe", delivery, event, err)
		return
	}
	if seen {
		h.count(func(m *Metrics) { m.Deduped.Add(1) })
		h.done(w, delivery, event, "duplicate delivery")
		return
	}

	dec, err := Filter(event, body)
	if err != nil {
		h.count(func(m *Metrics) { m.Errors.Add(1) })
		h.log().Warn("payload rejected", "delivery", delivery, "event", event, "err", err)
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}
	if !dec.Enqueue {
		h.count(func(m *Metrics) { m.Dropped.Add(1) })
		h.done(w, delivery, event, dec.Reason)
		return
	}
	env := dec.Envelope
	env.DeliveryID = delivery

	if ok, err := h.subs().HasSubscriber(ctx, env.Repo, env.PR); err != nil {
		// The pre-filter is an optimisation: losing it must not lose events.
		h.log().Warn("subscription pre-filter failed; enqueueing anyway",
			"delivery", delivery, "repo", env.Repo, "pr", env.PR, "err", err)
	} else if !ok {
		h.count(func(m *Metrics) { m.Unsubscribed.Add(1) })
		h.done(w, delivery, event, "no subscriber")
		return
	}

	if err := h.Queue.Enqueue(ctx, env); err != nil {
		if ferr := h.Dedupe.Forget(ctx, delivery); ferr != nil {
			h.log().Warn("dedupe cleanup failed; redelivery will be dropped",
				"delivery", delivery, "err", ferr)
		}
		h.fail(w, "enqueue", delivery, event, err)
		return
	}
	h.count(func(m *Metrics) { m.Enqueued.Add(1) })
	h.log().Info("enqueued", "delivery", delivery, "event", event,
		"kind", env.Kind, "repo", env.Repo, "pr", env.PR)
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintln(w, "enqueued")
}

// done acknowledges a delivery that produced no work with a 200 so GitHub
// does not retry it.
func (h *Handler) done(w http.ResponseWriter, delivery, event, reason string) {
	h.log().Info("dropped", "delivery", delivery, "event", event, "reason", reason)
	fmt.Fprintln(w, reason)
}

// fail reports an operational failure with a 500 so the delivery shows as
// failed in GitHub and can be redelivered.
func (h *Handler) fail(w http.ResponseWriter, stage, delivery, event string, err error) {
	h.count(func(m *Metrics) { m.Errors.Add(1) })
	h.log().Error(stage+" failed", "delivery", delivery, "event", event, "err", err)
	http.Error(w, stage+" failed", http.StatusInternalServerError)
}

func (h *Handler) subs() SubscriptionChecker {
	if h.Subs == nil {
		return AllowAll{}
	}
	return h.Subs
}

func (h *Handler) log() *slog.Logger {
	if h.Log == nil {
		return slog.Default()
	}
	return h.Log
}

func (h *Handler) count(f func(*Metrics)) {
	if h.Metrics != nil {
		f(h.Metrics)
	}
}
