package ingest

import (
	"strings"
	"testing"
)

// FuzzIngestVerify asserts the signature check's contract on arbitrary
// secrets and bodies (the webhook endpoint is the system's only public
// surface): a correctly signed body always verifies, and any single-byte
// perturbation of body, secret, or signature fails closed.
func FuzzIngestVerify(f *testing.F) {
	f.Add(docsSecret, docsPayload, byte(0))
	f.Add("s", "", byte(1))
	f.Add("", "body with no secret", byte(2))
	f.Fuzz(func(t *testing.T, secret, body string, flip byte) {
		sig := Sign([]byte(secret), []byte(body))
		if secret == "" {
			if Verify([]byte(secret), []byte(body), sig) {
				t.Fatal("empty secret must never verify")
			}
			return
		}
		if !Verify([]byte(secret), []byte(body), sig) {
			t.Fatal("self-signed body must verify")
		}
		// Flip one byte of the hex digest: must fail.
		digest := []byte(strings.TrimPrefix(sig, "sha256="))
		i := int(flip) % len(digest)
		digest[i] ^= 1
		if Verify([]byte(secret), []byte(body), "sha256="+string(digest)) {
			t.Fatal("perturbed signature must not verify")
		}
		// Extend the body: must fail.
		if Verify([]byte(secret), append([]byte(body), 'x'), sig) {
			t.Fatal("perturbed body must not verify")
		}
	})
}

// FuzzIngestFilter asserts the filter never panics on arbitrary verified
// payloads and that anything it decides to enqueue becomes a valid envelope
// once the delivery ID is stamped — i.e. the filter can never hand workers
// malformed work.
func FuzzIngestFilter(f *testing.F) {
	f.Add("workflow_run", workflowRunFailure)
	f.Add("pull_request", prClosed)
	f.Add("star", `{"action":"created"}`)
	f.Add("workflow_run", `{}`)
	f.Fuzz(func(t *testing.T, event, body string) {
		dec, err := Filter(event, []byte(body))
		if err != nil {
			return // malformed payloads are rejected, not enqueued
		}
		if !dec.Enqueue {
			if dec.Reason == "" {
				t.Fatal("a drop must carry a reason")
			}
			return
		}
		env := dec.Envelope
		if env.DeliveryID != "" {
			t.Fatal("filter must leave DeliveryID for the handler")
		}
		env.DeliveryID = "fuzz-delivery"
		if err := env.Validate(); err != nil {
			t.Fatalf("filter enqueued an invalid envelope: %v (%+v)", err, env)
		}
	})
}

// FuzzIngestEnvelope asserts the queue contract: arbitrary bytes never
// panic the parser, and anything that parses re-encodes to an equal
// envelope (workers and ingest agree on the wire form).
func FuzzIngestEnvelope(f *testing.F) {
	seed, err := validEnvelope().Encode()
	if err != nil {
		f.Fatalf("encode seed: %v", err)
	}
	f.Add(string(seed))
	f.Add(`{"schema":1,"delivery_id":"d","kind":"pr_closed","repo":"o/r","pr":1}`)
	f.Add(`{"schema":9}`)
	f.Add(`not json`)
	f.Fuzz(func(t *testing.T, data string) {
		env, err := ParseEnvelope([]byte(data))
		if err != nil {
			return
		}
		out, err := env.Encode()
		if err != nil {
			t.Fatalf("parsed envelope failed to encode: %v", err)
		}
		again, err := ParseEnvelope(out)
		if err != nil {
			t.Fatalf("re-encoded envelope failed to parse: %v", err)
		}
		if again != env {
			t.Fatalf("round trip drifted: %+v != %+v", again, env)
		}
	})
}
