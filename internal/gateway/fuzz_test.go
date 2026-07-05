package gateway

import (
	"strconv"
	"testing"
)

// FuzzGatewayClientFrame asserts the shim-frame parser never panics, that
// every accepted frame satisfies its type's invariants, and that accepted
// frames survive an encode → parse round trip.
func FuzzGatewayClientFrame(f *testing.F) {
	f.Add([]byte(`{"type":"hello","token":"t","session_id":"s"}`))
	f.Add([]byte(`{"type":"hello","token":"t","session_id":"s","last_event_id":"ev-9"}`))
	f.Add([]byte(`{"type":"subscribe","repo":"octo/repo","pr":7}`))
	f.Add([]byte(`{"type":"unsubscribe","repo":"octo/repo","pr":7}`))
	f.Add([]byte(`{"type":"ack","id":"ev-1"}`))
	f.Add([]byte(`{"type":"ping"}`))
	f.Add([]byte(`{"type":"event"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{nope`))
	f.Fuzz(func(t *testing.T, data []byte) {
		frame, err := ParseClientFrame(data)
		if err != nil {
			return
		}
		switch frame.Type {
		case FrameHello:
			if frame.Token == "" || frame.SessionID == "" {
				t.Fatalf("accepted hello missing credentials: %+v", frame)
			}
		case FrameSubscribe, FrameUnsubscribe:
			if frame.Repo == "" || frame.PR <= 0 {
				t.Fatalf("accepted %s missing target: %+v", frame.Type, frame)
			}
		case FrameAck:
			if frame.ID == "" {
				t.Fatalf("accepted ack missing id: %+v", frame)
			}
		case FramePing:
			// No required fields.
		default:
			t.Fatalf("accepted unknown frame type %q", frame.Type)
		}
		encoded, err := frame.Encode()
		if err != nil {
			t.Fatalf("Encode accepted frame: %v", err)
		}
		again, err := ParseClientFrame(encoded)
		if err != nil {
			t.Fatalf("re-parse encoded frame: %v", err)
		}
		if again != frame {
			t.Fatalf("round trip = %+v, want %+v", again, frame)
		}
	})
}

// FuzzGatewayDeliver asserts the deliver-body parser never panics, that
// accepted requests validate, and — the load-bearing safety property — that
// Suppressed never fires for ci_failure or pr_closed on any input.
func FuzzGatewayDeliver(f *testing.F) {
	f.Add([]byte(`{"schema":1,"event_id":"e","kind":"ci_failure","repo":"o/r","pr":1,"summary":"s"}`), "42")
	f.Add([]byte(`{"schema":1,"event_id":"e","kind":"review_comment","repo":"o/r","pr":1,"author":{"github_user_id":42,"login":"o"}}`), "42")
	f.Add([]byte(`{"schema":1,"event_id":"e","kind":"pr_closed","repo":"o/r","pr":1,"author":{"github_user_id":42}}`), "42")
	f.Add([]byte(`{"schema":1}`), "")
	f.Add([]byte(`{nope`), "0")
	f.Fuzz(func(t *testing.T, data []byte, userID string) {
		req, err := ParseDeliverRequest(data)
		if err != nil {
			return
		}
		if verr := req.Validate(); verr != nil {
			t.Fatalf("parsed request fails Validate: %v", verr)
		}
		suppressed := req.Suppressed(userID) // must be total
		if suppressed && (req.Kind == KindCIFailure || req.Kind == KindPRClosed) {
			t.Fatalf("suppressed a %s event — CI failures and PR closes must always deliver", req.Kind)
		}
		if suppressed {
			if req.Author == nil || req.Author.GitHubUserID == 0 {
				t.Fatalf("suppressed without a numeric author: %+v", req)
			}
			if userID != strconv.FormatInt(req.Author.GitHubUserID, 10) {
				t.Fatalf("suppressed user %q for author %d", userID, req.Author.GitHubUserID)
			}
		}
		encoded, err := req.Encode()
		if err != nil {
			t.Fatalf("Encode accepted request: %v", err)
		}
		again, err := ParseDeliverRequest(encoded)
		if err != nil {
			t.Fatalf("re-parse encoded request: %v", err)
		}
		if again.EventID != req.EventID || again.Kind != req.Kind || again.Repo != req.Repo || again.PR != req.PR {
			t.Fatalf("round trip = %+v, want %+v", again, req)
		}
	})
}
