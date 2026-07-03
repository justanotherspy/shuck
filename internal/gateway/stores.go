package gateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrTokenNotFound reports an unknown or revoked bearer token. Connections
// presenting one are closed with CloseUnauthorized.
var ErrTokenNotFound = errors.New("gateway: token not found")

// SubscriberKey identifies one shim connection: the token owner's numeric
// GitHub user ID plus the Claude Code session ID. Session IDs are
// client-supplied and untrusted — namespacing them under the authenticated
// user is what makes presenting someone else's session ID yield nothing.
type SubscriberKey struct {
	UserID    string // decimal github_user_id from the token row
	SessionID string
}

// String returns the canonical "user_id#session_id" form used as the
// DynamoDB key.
func (k SubscriberKey) String() string {
	return k.UserID + "#" + k.SessionID
}

// ParseSubscriberKey parses the canonical "user_id#session_id" form.
func ParseSubscriberKey(s string) (SubscriberKey, error) {
	user, session, ok := strings.Cut(s, "#")
	if !ok || user == "" || session == "" {
		return SubscriberKey{}, fmt.Errorf("invalid subscriber key %q", s)
	}
	return SubscriberKey{UserID: user, SessionID: session}, nil
}

// TokenRecord is a row in the token table, written by the JUS-90 portal and
// read here at hello time. The repo allowlist is reserved and not enforced
// in v1.
type TokenRecord struct {
	GitHubUserID int64
	GitHubLogin  string
}

// TokenStore resolves bearer-token hashes to their owners.
type TokenStore interface {
	// Lookup resolves the lowercase hex sha256 of a bearer token to its
	// record, or ErrTokenNotFound for unknown/revoked tokens.
	Lookup(ctx context.Context, tokenHash string) (TokenRecord, error)
}

// PRRef names one pull request.
type PRRef struct {
	Repo string // owner/name
	PR   int
}

// String returns the canonical "owner/name#pr" form used as the DynamoDB
// partition key.
func (r PRRef) String() string {
	return fmt.Sprintf("%s#%d", r.Repo, r.PR)
}

// ParsePRRef parses the canonical "owner/name#pr" form.
func ParsePRRef(s string) (PRRef, error) {
	i := strings.LastIndex(s, "#")
	if i <= 0 {
		return PRRef{}, fmt.Errorf("invalid pr ref %q", s)
	}
	pr, err := strconv.Atoi(s[i+1:])
	if err != nil || pr <= 0 {
		return PRRef{}, fmt.Errorf("invalid pr ref %q", s)
	}
	return PRRef{Repo: s[:i], PR: pr}, nil
}

// SubscriptionStore owns the repo#pr ↔ subscriber mapping.
type SubscriptionStore interface {
	// Subscribe records sub's interest in ref. Idempotent.
	Subscribe(ctx context.Context, ref PRRef, sub SubscriberKey) error
	// Unsubscribe removes one subscription. Removing a missing one is not
	// an error.
	Unsubscribe(ctx context.Context, ref PRRef, sub SubscriberKey) error
	// Subscribers lists every subscriber of ref, for deliver fan-out.
	Subscribers(ctx context.Context, ref PRRef) ([]SubscriberKey, error)
	// BySubscriber lists sub's PRs via the reverse index, for the sweep.
	BySubscriber(ctx context.Context, sub SubscriberKey) ([]PRRef, error)
	// RemoveAllForPR drops every subscription for ref (PR closed/merged).
	RemoveAllForPR(ctx context.Context, ref PRRef) error
	// RemoveAllForSubscriber drops every subscription held by sub (sweep).
	RemoveAllForSubscriber(ctx context.Context, sub SubscriberKey) error
}

// EventBuffer is the per-subscriber durable event queue: rows are written
// before any push (write-then-push), replayed after reconnects, and deleted
// on ack.
type EventBuffer interface {
	// Append allocates the next seq and persists ev atomically with an
	// event-id dedupe marker. duplicate reports that ev.ID was already
	// buffered for sub, in which case nothing was written and seq is the
	// existing one.
	Append(ctx context.Context, sub SubscriberKey, ev Event) (seq int64, duplicate bool, err error)
	// After returns buffered events with seq > afterSeq in ascending seq
	// order. afterSeq 0 means everything.
	After(ctx context.Context, sub SubscriberKey, afterSeq int64) ([]Event, error)
	// SeqOf resolves a wire event id to its seq via the dedupe marker.
	// ok=false when the id is unknown or expired — callers degrade to a
	// full replay, which the shim dedupes by event id.
	SeqOf(ctx context.Context, sub SubscriberKey, eventID string) (seq int64, ok bool, err error)
	// Ack deletes the buffer row for eventID. Acking an unknown id is not
	// an error (the row may have expired).
	Ack(ctx context.Context, sub SubscriberKey, eventID string) error
	// Purge removes every row of sub's partition (grace-window sweep).
	Purge(ctx context.Context, sub SubscriberKey) error
}

// PresenceStore records when a subscriber was last connected, durably, so
// the grace-window sweep survives gateway restarts.
type PresenceStore interface {
	// Touch records sub as connected and active at t. Called on connect
	// and periodically while the connection lives.
	Touch(ctx context.Context, sub SubscriberKey, at time.Time) error
	// MarkDisconnected records that sub's connection closed at t.
	MarkDisconnected(ctx context.Context, sub SubscriberKey, at time.Time) error
	// Stale lists subscribers with no activity since cutoff — neither a
	// touch nor a disconnect newer than it. Live connections are excluded
	// by the caller via the registry, not here.
	Stale(ctx context.Context, cutoff time.Time) ([]SubscriberKey, error)
	// Delete removes sub's presence row after a sweep.
	Delete(ctx context.Context, sub SubscriberKey) error
}
