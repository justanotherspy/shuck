package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// DeliverSchema is the version of the worker → gateway deliver contract.
// The gateway rejects requests with a different schema instead of guessing.
const DeliverSchema = 1

// EventKind classifies a delivered event. The full enum exists now because
// self-authored suppression is kind-scoped; JUS-91 starts producing the
// review kinds.
type EventKind string

// The event kinds the gateway delivers.
const (
	// KindCIFailure carries a distilled CI failure. It is never suppressed.
	KindCIFailure EventKind = "ci_failure"
	// KindPRClosed is the final informational event for a merged or closed
	// PR; delivering it also removes every subscription for the PR.
	KindPRClosed EventKind = "pr_closed"
	// KindReviewComment carries a preprocessed review comment (JUS-91).
	KindReviewComment EventKind = "review_comment"
	// KindReview carries a preprocessed review verdict (JUS-91).
	KindReview EventKind = "review"
)

// valid reports whether k is a known event kind.
func (k EventKind) valid() bool {
	switch k {
	case KindCIFailure, KindPRClosed, KindReviewComment, KindReview:
		return true
	}
	return false
}

// Author identifies who caused an event, for self-authored suppression.
// Matching uses the immutable numeric GitHub user ID only — logins are
// display data and can be reassigned.
type Author struct {
	GitHubUserID int64  `json:"github_user_id"`
	Login        string `json:"login"`
}

// DeliverRequest is the body of POST /internal/deliver — the contract the
// JUS-87 workers produce. EventID is the worker-side idempotency key:
// redelivering the same event_id to the same subscriber buffers it once.
type DeliverRequest struct {
	Schema  int       `json:"schema"`
	EventID string    `json:"event_id"`
	Kind    EventKind `json:"kind"`
	Repo    string    `json:"repo"` // owner/name
	PR      int       `json:"pr"`
	Summary string    `json:"summary"`
	Author  *Author   `json:"author,omitempty"`
}

// Encode marshals the request to its wire form.
func (r DeliverRequest) Encode() ([]byte, error) {
	return json.Marshal(r)
}

// Validate reports whether the request satisfies the schema contract.
func (r DeliverRequest) Validate() error {
	switch {
	case r.Schema != DeliverSchema:
		return fmt.Errorf("unsupported deliver schema %d (want %d)", r.Schema, DeliverSchema)
	case r.EventID == "":
		return errors.New("deliver request missing event_id")
	case r.Repo == "":
		return errors.New("deliver request missing repo")
	case r.PR <= 0:
		return fmt.Errorf("deliver request has invalid pr %d", r.PR)
	case !r.Kind.valid():
		return fmt.Errorf("unknown deliver kind %q", r.Kind)
	}
	return nil
}

// ParseDeliverRequest decodes and validates a deliver body. It is the
// gateway-side counterpart of Encode.
func ParseDeliverRequest(data []byte) (DeliverRequest, error) {
	var r DeliverRequest
	if err := json.Unmarshal(data, &r); err != nil {
		return DeliverRequest{}, fmt.Errorf("decode deliver request: %w", err)
	}
	if err := r.Validate(); err != nil {
		return DeliverRequest{}, err
	}
	return r, nil
}

// Suppressed reports whether the event must be skipped for the subscriber
// whose token row resolved to userID (decimal GitHub user ID). Suppression
// is kind-scoped — review events only, never CI failures or PR closes — and
// matches on the numeric ID alone: no author, a zero ID, or a login-only
// coincidence never suppresses.
func (r DeliverRequest) Suppressed(userID string) bool {
	if r.Kind != KindReviewComment && r.Kind != KindReview {
		return false
	}
	if r.Author == nil || r.Author.GitHubUserID == 0 {
		return false
	}
	return userID == strconv.FormatInt(r.Author.GitHubUserID, 10)
}

// Event returns the wire event frame for this request. Seq is stamped by
// the buffer at append time.
func (r DeliverRequest) Event() Event {
	return Event{
		Type:    frameEvent,
		ID:      r.EventID,
		Repo:    r.Repo,
		PR:      r.PR,
		Kind:    r.Kind,
		Summary: r.Summary,
	}
}
