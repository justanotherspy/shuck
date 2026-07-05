package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// MaxFrameSize caps shim → gateway frames. Client frames are tiny (a token,
// a repo ref, an event id); 32 KiB leaves generous headroom while bounding
// memory per connection.
const MaxFrameSize = 32 << 10

// Client frame types (shim → gateway).
const (
	// FrameHello authenticates and identifies the connection. It must be
	// the first frame and must not repeat.
	FrameHello = "hello"
	// FrameSubscribe subscribes the session to a PR's events.
	FrameSubscribe = "subscribe"
	// FrameUnsubscribe removes a PR subscription.
	FrameUnsubscribe = "unsubscribe"
	// FrameAck confirms an event was consumed; its buffer row is deleted.
	FrameAck = "ack"
	// FramePing is an application-level keepalive. The serverless gateway
	// needs it (API Gateway's idle timeout counts data frames, and Lambda
	// handlers never see protocol pings); the resident gateway treats it as
	// a no-op so one shim speaks to both.
	FramePing = "ping"
)

// frameEvent is the only gateway → shim frame type.
const frameEvent = "event"

// Application close codes (4000-range per RFC 6455). Drain uses the
// standard going-away code (1001) so shims reconnect after deploys.
const (
	// CloseUnauthorized rejects a connection: bad handshake frame or an
	// unknown/revoked token.
	CloseUnauthorized = 4401
	// CloseReplaced closes the older connection when the same subscriber
	// key connects again (newest wins).
	CloseReplaced = 4409
)

// ClientFrame is any shim → gateway frame; Type discriminates and decides
// which other fields are meaningful.
type ClientFrame struct {
	Type string `json:"type"`
	// Hello fields.
	Token       string `json:"token,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	LastEventID string `json:"last_event_id,omitempty"`
	// Subscribe / unsubscribe fields.
	Repo string `json:"repo,omitempty"` // owner/name
	PR   int    `json:"pr,omitempty"`
	// Ack field: the event id being acknowledged.
	ID string `json:"id,omitempty"`
}

// Encode marshals the frame to its wire form. It is what the shim sends and
// what the tests dial with.
func (f ClientFrame) Encode() ([]byte, error) {
	return json.Marshal(f)
}

// ParseClientFrame decodes and validates one shim frame. Unknown types and
// frames missing their type's required fields are rejected.
func ParseClientFrame(data []byte) (ClientFrame, error) {
	var f ClientFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return ClientFrame{}, fmt.Errorf("decode client frame: %w", err)
	}
	switch f.Type {
	case FrameHello:
		if f.Token == "" {
			return ClientFrame{}, errors.New("hello frame missing token")
		}
		if f.SessionID == "" {
			return ClientFrame{}, errors.New("hello frame missing session_id")
		}
	case FrameSubscribe, FrameUnsubscribe:
		if f.Repo == "" {
			return ClientFrame{}, fmt.Errorf("%s frame missing repo", f.Type)
		}
		if f.PR <= 0 {
			return ClientFrame{}, fmt.Errorf("%s frame has invalid pr %d", f.Type, f.PR)
		}
	case FrameAck:
		if f.ID == "" {
			return ClientFrame{}, errors.New("ack frame missing id")
		}
	case FramePing:
		// No required fields: a bare keepalive.
	default:
		return ClientFrame{}, fmt.Errorf("unknown client frame type %q", f.Type)
	}
	return f, nil
}

// Event is the gateway → shim event frame and the payload persisted in the
// buffer. ID is the worker event_id (the shim's dedupe and ack key); Seq is
// the per-subscriber sequence the buffer stamps at append time.
type Event struct {
	Type    string    `json:"type"` // always "event" on the wire
	ID      string    `json:"id"`
	Seq     int64     `json:"seq"`
	Repo    string    `json:"repo"`
	PR      int       `json:"pr"`
	Kind    EventKind `json:"kind"`
	Summary string    `json:"summary"`
}

// Encode marshals the event to its wire form.
func (e Event) Encode() ([]byte, error) {
	e.Type = frameEvent
	return json.Marshal(e)
}

// HashToken returns the lowercase hex SHA-256 of a bearer token — the token
// table's partition key. The raw token is never stored or logged.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
