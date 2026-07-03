package gateway

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

// DeliverSecretHeader authenticates worker → gateway deliver calls. The
// endpoint is never public, but topology alone is not the protection —
// network controls are optional in the deployment charts.
const DeliverSecretHeader = "X-Shuck-Deliver-Secret" //nolint:gosec // header name, not a credential

// DefaultDeliverMaxBody caps the deliver request body when
// DeliverHandler.MaxBody is unset. Summaries are a few KB; 1 MiB is
// generous headroom.
const DefaultDeliverMaxBody = 1 << 20

// DeliverHandler is the POST /internal/deliver http.Handler.
type DeliverHandler struct {
	// Secrets holds one or two accepted values; the second exists so a
	// rotation can roll workers without a hard cutover.
	Secrets [][]byte
	Hub     *Hub
	// Log may be nil, which means slog.Default().
	Log *slog.Logger
	// Metrics may be nil, which disables counting.
	Metrics *Metrics
	// MaxBody caps the request body size; 0 means DefaultDeliverMaxBody.
	MaxBody int64
}

// ServeHTTP authenticates, parses, and fans out one deliver call. The
// secret check runs before the body is read or any store is touched, so a
// rejected call can never buffer anything.
func (d *DeliverHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		plainError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !d.authorized(r.Header.Get(DeliverSecretHeader)) {
		d.count(func(m *Metrics) { m.DeliverRejected.Add(1) })
		plainError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	maxBody := d.MaxBody
	if maxBody <= 0 {
		maxBody = DefaultDeliverMaxBody
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		plainError(w, "read body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxBody {
		plainError(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	req, err := ParseDeliverRequest(body)
	if err != nil {
		// A schema violation is a worker bug; retrying the same payload
		// cannot help, so it is a 4xx.
		d.log().Warn("invalid deliver request", "err", err)
		plainError(w, "invalid deliver request", http.StatusBadRequest)
		return
	}
	d.count(func(m *Metrics) { m.DeliverRequests.Add(1) })
	res, err := d.Hub.Deliver(r.Context(), req)
	if err != nil {
		// Partial or total failure: the worker retries and the event_id
		// dedupe marker absorbs the subscribers already buffered.
		d.log().Error("deliver failed", "id", req.EventID, "err", err)
		plainError(w, "deliver failed", http.StatusInternalServerError)
		return
	}
	d.log().Info("delivered", "id", req.EventID, "kind", string(req.Kind),
		"subscribers", res.Subscribers, "buffered", res.Buffered,
		"pushed", res.Pushed, "suppressed", res.Suppressed, "deduped", res.Deduped)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(res)
}

// authorized compares the presented secret against every configured value
// in constant time, without early exit, so timing reveals nothing about
// which (if any) value matched.
func (d *DeliverHandler) authorized(presented string) bool {
	if presented == "" || len(d.Secrets) == 0 {
		return false
	}
	p := []byte(presented)
	ok := 0
	for _, secret := range d.Secrets {
		if len(secret) == 0 {
			continue
		}
		ok |= subtle.ConstantTimeCompare(p, secret)
	}
	return ok == 1
}

// plainError writes a static error string; deliver responses never reflect
// request-derived content.
func plainError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg+"\n")
}

func (d *DeliverHandler) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d *DeliverHandler) count(f func(*Metrics)) {
	if d.Metrics != nil {
		f(d.Metrics)
	}
}
