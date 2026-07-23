package ingest

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeDedupe struct {
	seen      map[string]bool
	seenErr   error
	forgetErr error
	forgot    []string
}

func (f *fakeDedupe) Seen(_ context.Context, id string) (bool, error) {
	if f.seenErr != nil {
		return false, f.seenErr
	}
	if f.seen[id] {
		return true, nil
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	f.seen[id] = true
	return false, nil
}

func (f *fakeDedupe) Forget(ctx context.Context, id string) error {
	f.forgot = append(f.forgot, id)
	if f.forgetErr != nil {
		return f.forgetErr
	}
	// A real store cannot delete over a dead context — this is what makes
	// the WithoutCancel regression test bite.
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(f.seen, id)
	return nil
}

type fakeQueue struct {
	err error
	got []Envelope
}

func (f *fakeQueue) Enqueue(_ context.Context, env Envelope) error {
	if f.err != nil {
		return f.err
	}
	f.got = append(f.got, env)
	return nil
}

type fakeSubs struct {
	ok  bool
	err error
}

func (f fakeSubs) HasSubscriber(context.Context, string, int) (bool, error) {
	return f.ok, f.err
}

// prSubs subscribes only the listed PR numbers (any repo).
type prSubs map[int]bool

func (s prSubs) HasSubscriber(_ context.Context, _ string, pr int) (bool, error) {
	return s[pr], nil
}

// enqueueFunc adapts a function to the Enqueuer interface.
type enqueueFunc func(context.Context, Envelope) error

func (f enqueueFunc) Enqueue(ctx context.Context, env Envelope) error {
	return f(ctx, env)
}

const testSecret = "hooksecret"

func newHandler(d Deduper, q Enqueuer, s SubscriptionChecker) *Handler {
	return &Handler{
		Secret:  []byte(testSecret),
		Dedupe:  d,
		Queue:   q,
		Subs:    s,
		Log:     slog.New(slog.DiscardHandler),
		Metrics: &Metrics{},
	}
}

// post signs body and runs it through the handler as a webhook delivery.
func post(t *testing.T, h *Handler, event, delivery, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set(SignatureHeader, Sign([]byte(testSecret), []byte(body)))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	if mutate != nil {
		mutate(req)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestHandlerEnqueuesFailure(t *testing.T) {
	q := &fakeQueue{}
	h := newHandler(&fakeDedupe{}, q, nil) // nil Subs = AllowAll
	rr := post(t, h, "workflow_run", "d-1", workflowRunFailure, nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rr.Code, rr.Body)
	}
	// The fixture run is associated with PRs 9 and 10: one envelope each.
	if len(q.got) != 2 {
		t.Fatalf("enqueued %d envelopes, want 2 (one per associated PR)", len(q.got))
	}
	for i, pr := range []int{9, 10} {
		env := q.got[i]
		if env.DeliveryID != "d-1" || env.Kind != KindCIFailure || env.PR != pr {
			t.Fatalf("envelope[%d] = %+v, want delivery d-1 / %s / pr %d", i, env, KindCIFailure, pr)
		}
		if err := env.Validate(); err != nil {
			t.Fatalf("enqueued envelope invalid: %v", err)
		}
	}
	if got := h.Metrics.Enqueued.Load(); got != 2 {
		t.Fatalf("Enqueued metric = %d, want 2 (per envelope)", got)
	}
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	h := newHandler(&fakeDedupe{}, &fakeQueue{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/webhook", http.NoBody)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestHandlerRejectsBadSignature(t *testing.T) {
	q := &fakeQueue{}
	h := newHandler(&fakeDedupe{}, q, nil)
	rr := post(t, h, "workflow_run", "d-1", workflowRunFailure, func(r *http.Request) {
		r.Header.Set(SignatureHeader, Sign([]byte("wrong secret"), []byte(workflowRunFailure)))
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if len(q.got) != 0 {
		t.Fatal("nothing may be enqueued for an unverified delivery")
	}
}

func TestHandlerRejectsMissingHeaders(t *testing.T) {
	h := newHandler(&fakeDedupe{}, &fakeQueue{}, nil)
	rr := post(t, h, "workflow_run", "", workflowRunFailure, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing delivery: status = %d, want 400", rr.Code)
	}
	rr = post(t, h, "", "d-1", workflowRunFailure, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing event: status = %d, want 400", rr.Code)
	}
}

func TestHandlerDropsDuplicateDelivery(t *testing.T) {
	q := &fakeQueue{}
	h := newHandler(&fakeDedupe{}, q, nil)
	if rr := post(t, h, "workflow_run", "d-1", workflowRunFailure, nil); rr.Code != http.StatusAccepted {
		t.Fatalf("first delivery: status = %d", rr.Code)
	}
	rr := post(t, h, "workflow_run", "d-1", workflowRunFailure, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("replay: status = %d, want 200", rr.Code)
	}
	if len(q.got) != 2 {
		t.Fatalf("replay enqueued again: %d envelopes, want the first delivery's 2", len(q.got))
	}
	if got := h.Metrics.Deduped.Load(); got != 1 {
		t.Fatalf("Deduped metric = %d, want 1", got)
	}
}

func TestHandlerDropsFilteredEvent(t *testing.T) {
	q := &fakeQueue{}
	h := newHandler(&fakeDedupe{}, q, nil)
	body := strings.Replace(workflowRunFailure, `"failure"`, `"success"`, 1)
	rr := post(t, h, "workflow_run", "d-1", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if len(q.got) != 0 {
		t.Fatal("filtered event must not enqueue")
	}
	if got := h.Metrics.Dropped.Load(); got != 1 {
		t.Fatalf("Dropped metric = %d, want 1", got)
	}
}

func TestHandlerResponsesArePlainTextAndStatic(t *testing.T) {
	// Payload-derived strings (event/action names) must never be reflected
	// into the response — they go to the log only — and every response pins
	// text/plain + nosniff, so nothing can be rendered as HTML (CodeQL:
	// reflected XSS).
	q := &fakeQueue{}
	h := newHandler(&fakeDedupe{}, q, nil)
	tainted := "<script>alert(1)</script>"
	dropped := post(t, h, tainted, "d-1", `{"action":"`+tainted+`","repository":{"full_name":"o/r"}}`, nil)
	enqueued := post(t, h, "workflow_run", "d-2", workflowRunFailure, nil)
	if body := dropped.Body.String(); body != "ignored\n" {
		t.Errorf("dropped response body = %q, want the static %q", body, "ignored\n")
	}
	for name, rr := range map[string]*httptest.ResponseRecorder{"dropped": dropped, "enqueued": enqueued} {
		if strings.Contains(rr.Body.String(), tainted) {
			t.Errorf("%s response reflects request-derived text: %q", name, rr.Body)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
			t.Errorf("%s response Content-Type = %q", name, ct)
		}
		if opt := rr.Header().Get("X-Content-Type-Options"); opt != "nosniff" {
			t.Errorf("%s response X-Content-Type-Options = %q", name, opt)
		}
	}
}

func TestHandlerRejectsMalformedPayload(t *testing.T) {
	h := newHandler(&fakeDedupe{}, &fakeQueue{}, nil)
	rr := post(t, h, "workflow_run", "d-1", "{not json", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandlerSubscriptionPreFilter(t *testing.T) {
	q := &fakeQueue{}
	h := newHandler(&fakeDedupe{}, q, fakeSubs{ok: false})
	rr := post(t, h, "workflow_run", "d-1", workflowRunFailure, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); body != "no subscriber\n" {
		t.Fatalf("body = %q, want %q", body, "no subscriber\n")
	}
	if len(q.got) != 0 {
		t.Fatal("unsubscribed PRs must not enqueue")
	}
	if got := h.Metrics.Unsubscribed.Load(); got != 2 {
		t.Fatalf("Unsubscribed metric = %d, want 2 (per dropped envelope)", got)
	}
}

func TestHandlerSubscriptionPreFilterPerEnvelope(t *testing.T) {
	// The fixture run touches PRs 9 and 10; with a subscriber on 10 only,
	// exactly that envelope must be enqueued — dropping the whole delivery
	// because its *first* PR had no subscriber would lose the event.
	q := &fakeQueue{}
	h := newHandler(&fakeDedupe{}, q, prSubs{10: true})
	rr := post(t, h, "workflow_run", "d-1", workflowRunFailure, nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rr.Code, rr.Body)
	}
	if len(q.got) != 1 || q.got[0].PR != 10 {
		t.Fatalf("enqueued %+v, want exactly the PR 10 envelope", q.got)
	}
	if got := h.Metrics.Unsubscribed.Load(); got != 1 {
		t.Fatalf("Unsubscribed metric = %d, want 1 (the PR 9 envelope)", got)
	}
	if got := h.Metrics.Enqueued.Load(); got != 1 {
		t.Fatalf("Enqueued metric = %d, want 1", got)
	}
}

func TestHandlerSubscriptionCheckFailsOpen(t *testing.T) {
	q := &fakeQueue{}
	h := newHandler(&fakeDedupe{}, q, fakeSubs{err: errors.New("ddb down")})
	rr := post(t, h, "workflow_run", "d-1", workflowRunFailure, nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (pre-filter must fail open)", rr.Code)
	}
	if len(q.got) != 2 {
		t.Fatalf("enqueued %d envelopes, want 2: events lost to a broken pre-filter", len(q.got))
	}
}

func TestHandlerDedupeError(t *testing.T) {
	h := newHandler(&fakeDedupe{seenErr: errors.New("ddb down")}, &fakeQueue{}, nil)
	rr := post(t, h, "workflow_run", "d-1", workflowRunFailure, nil)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestHandlerEnqueueErrorForgetsDedupe(t *testing.T) {
	d := &fakeDedupe{}
	h := newHandler(d, &fakeQueue{err: errors.New("sqs down")}, nil)
	rr := post(t, h, "workflow_run", "d-1", workflowRunFailure, nil)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if len(d.forgot) != 1 || d.forgot[0] != "d-1" {
		t.Fatalf("dedupe row not cleaned up (forgot=%v); a redelivery would be lost", d.forgot)
	}
	// Forget failing too must not change the response (already a 500).
	d = &fakeDedupe{forgetErr: errors.New("also down")}
	h = newHandler(d, &fakeQueue{err: errors.New("sqs down")}, nil)
	if rr := post(t, h, "workflow_run", "d-2", workflowRunFailure, nil); rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestHandlerForgetSurvivesCanceledRequest(t *testing.T) {
	// GitHub abandons webhook deliveries after ~10s. If the request context
	// dies while the enqueue is failing, the dedupe cleanup must still run —
	// otherwise the GUID stays recorded, GitHub's redelivery is answered
	// "duplicate", and the event is lost forever.
	d := &fakeDedupe{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHandler(d, enqueueFunc(func(context.Context, Envelope) error {
		cancel() // the client gave up while the enqueue was in flight
		return context.Canceled
	}), nil)
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(workflowRunFailure)).WithContext(ctx)
	req.Header.Set(SignatureHeader, Sign([]byte(testSecret), []byte(workflowRunFailure)))
	req.Header.Set("X-GitHub-Event", "workflow_run")
	req.Header.Set("X-GitHub-Delivery", "d-1")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if len(d.forgot) != 1 {
		t.Fatalf("Forget called %d times, want 1", len(d.forgot))
	}
	if d.seen["d-1"] {
		t.Fatal("dedupe row survived the canceled request; the redelivery below would be dropped")
	}

	// GitHub redelivers the same GUID; with the queue healthy again it must
	// be processed, not dropped as a duplicate.
	q := &fakeQueue{}
	h.Queue = q
	if rr := post(t, h, "workflow_run", "d-1", workflowRunFailure, nil); rr.Code != http.StatusAccepted {
		t.Fatalf("redelivery status = %d, want 202 (%s)", rr.Code, rr.Body)
	}
	if len(q.got) != 2 {
		t.Fatalf("redelivery enqueued %d envelopes, want 2", len(q.got))
	}
}

// errReader fails every read, simulating a client that dies mid-body.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestHandlerBodyReadError(t *testing.T) {
	h := newHandler(&fakeDedupe{}, &fakeQueue{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/webhook", errReader{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandlerDuplicateMalformedPayload(t *testing.T) {
	// Dedupe precedes the filter: a redelivery of a GUID whose first attempt
	// was malformed is answered as a duplicate, not re-parsed.
	h := newHandler(&fakeDedupe{}, &fakeQueue{}, nil)
	if rr := post(t, h, "workflow_run", "d-1", "{not json", nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("first delivery: status = %d, want 400", rr.Code)
	}
	rr := post(t, h, "workflow_run", "d-1", "{not json", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("replay: status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); body != "duplicate delivery\n" {
		t.Fatalf("replay body = %q, want %q", body, "duplicate delivery\n")
	}
	if got := h.Metrics.Deduped.Load(); got != 1 {
		t.Fatalf("Deduped metric = %d, want 1", got)
	}
}

func TestHandlerRejectsOversizedBody(t *testing.T) {
	h := newHandler(&fakeDedupe{}, &fakeQueue{}, nil)
	h.MaxBody = 16
	rr := post(t, h, "workflow_run", "d-1", workflowRunFailure, nil)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rr.Code)
	}
}

func TestHandlerNilOptionalsAreSafe(t *testing.T) {
	// Nil Subs, Log, and Metrics must all default rather than panic.
	h := &Handler{Secret: []byte(testSecret), Dedupe: &fakeDedupe{}, Queue: &fakeQueue{}}
	rr := post(t, h, "workflow_run", "d-1", workflowRunFailure, nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
}
