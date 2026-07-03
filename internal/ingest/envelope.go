// Package ingest implements the stateless webhook ingest core of shuck's
// opt-in self-hosted mode (JUS-86): verify the GitHub webhook signature,
// dedupe the delivery, filter to the event kinds workers care about, and
// enqueue a slim envelope describing the work. The package is pure — the AWS
// adapters live in ingest/awsx and the binary in cmd/shuck-ingest — so the
// portable shuck CLI never links any of it (see docs/V2.md for the
// compatibility contract).
package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
)

// EnvelopeSchema is the version of the queue envelope contract. Workers
// reject envelopes with a different schema instead of guessing.
const EnvelopeSchema = 1

// Kind is the type of work an envelope requests from a worker.
type Kind string

// The envelope kinds produced by the v1 filter. JUS-91 adds the review
// kinds.
const (
	// KindCIFailure asks a worker to fetch and distil a failed workflow run.
	KindCIFailure Kind = "ci_failure"
	// KindPRClosed needs no fetch: the worker passes it straight to the
	// gateway so subscriptions for the PR are cleaned up (JUS-88).
	KindPRClosed Kind = "pr_closed"
)

// Envelope is the slim message enqueued for workers — identifiers only,
// never the raw webhook payload. It is the contract JUS-87 consumes.
type Envelope struct {
	Schema         int    `json:"schema"`
	DeliveryID     string `json:"delivery_id"`
	Kind           Kind   `json:"kind"`
	Repo           string `json:"repo"` // owner/name
	PR             int    `json:"pr"`
	RunID          int64  `json:"run_id,omitempty"`
	HeadSHA        string `json:"head_sha,omitempty"`
	InstallationID int64  `json:"installation_id,omitempty"`
}

// Encode marshals the envelope to its queue wire form.
func (e Envelope) Encode() ([]byte, error) {
	return json.Marshal(e)
}

// Validate reports whether the envelope satisfies the schema contract.
func (e Envelope) Validate() error {
	switch {
	case e.Schema != EnvelopeSchema:
		return fmt.Errorf("unsupported envelope schema %d (want %d)", e.Schema, EnvelopeSchema)
	case e.DeliveryID == "":
		return errors.New("envelope missing delivery_id")
	case e.Repo == "":
		return errors.New("envelope missing repo")
	case e.PR <= 0:
		return fmt.Errorf("envelope has invalid pr %d", e.PR)
	}
	switch e.Kind {
	case KindCIFailure:
		if e.RunID <= 0 {
			return fmt.Errorf("ci_failure envelope has invalid run_id %d", e.RunID)
		}
	case KindPRClosed:
	default:
		return fmt.Errorf("unknown envelope kind %q", e.Kind)
	}
	return nil
}

// ParseEnvelope decodes and validates a queue message body. It is the
// worker-side counterpart of Encode.
func ParseEnvelope(data []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}
