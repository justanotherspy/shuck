package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseClientFrame(t *testing.T) {
	cases := []struct {
		name    string
		frame   string
		wantErr string // substring; empty means valid
	}{
		{"hello", `{"type":"hello","token":"t","session_id":"s"}`, ""},
		{"hello with cursor", `{"type":"hello","token":"t","session_id":"s","last_event_id":"ev-9"}`, ""},
		{"hello without token", `{"type":"hello","session_id":"s"}`, "missing token"},
		{"hello without session", `{"type":"hello","token":"t"}`, "missing session_id"},
		{"subscribe", `{"type":"subscribe","repo":"octo/repo","pr":7}`, ""},
		{"subscribe without repo", `{"type":"subscribe","pr":7}`, "missing repo"},
		{"subscribe bad pr", `{"type":"subscribe","repo":"octo/repo","pr":0}`, "invalid pr"},
		{"unsubscribe", `{"type":"unsubscribe","repo":"octo/repo","pr":7}`, ""},
		{"unsubscribe negative pr", `{"type":"unsubscribe","repo":"octo/repo","pr":-1}`, "invalid pr"},
		{"ack", `{"type":"ack","id":"ev-1"}`, ""},
		{"ack without id", `{"type":"ack"}`, "missing id"},
		{"unknown type", `{"type":"event"}`, "unknown client frame type"},
		{"empty type", `{}`, "unknown client frame type"},
		{"not json", `{nope`, "decode client frame"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := ParseClientFrame([]byte(tc.frame))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseClientFrame: %v", err)
				}
				data, err := frame.Encode()
				if err != nil {
					t.Fatalf("Encode: %v", err)
				}
				again, err := ParseClientFrame(data)
				if err != nil {
					t.Fatalf("re-parse: %v", err)
				}
				if again != frame {
					t.Fatalf("round trip = %+v, want %+v", again, frame)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ParseClientFrame = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestEventEncodeStampsType(t *testing.T) {
	ev := Event{ID: "ev-1", Seq: 3, Repo: "octo/repo", PR: 7, Kind: KindCIFailure, Summary: "boom"}
	data, err := ev.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var got Event
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != "event" {
		t.Fatalf("wire type = %q, want %q", got.Type, "event")
	}
	if got.ID != ev.ID || got.Seq != ev.Seq || got.Kind != ev.Kind || got.Summary != ev.Summary {
		t.Fatalf("round trip = %+v, want %+v", got, ev)
	}
}

func TestHashToken(t *testing.T) {
	// sha256("hello") — a fixed vector so the table key format can never
	// drift silently.
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got := HashToken("hello"); got != want {
		t.Fatalf("HashToken = %q, want %q", got, want)
	}
	if HashToken("hello") == HashToken("hello2") {
		t.Fatal("distinct tokens must not collide")
	}
}

func TestSubscriberKey(t *testing.T) {
	key := SubscriberKey{UserID: "42", SessionID: "sess-1"}
	if key.String() != "42#sess-1" {
		t.Fatalf("String = %q", key.String())
	}
	parsed, err := ParseSubscriberKey("42#sess-1")
	if err != nil {
		t.Fatalf("ParseSubscriberKey: %v", err)
	}
	if parsed != key {
		t.Fatalf("parsed = %+v, want %+v", parsed, key)
	}
	for _, bad := range []string{"", "42", "#sess", "42#"} {
		if _, err := ParseSubscriberKey(bad); err == nil {
			t.Fatalf("ParseSubscriberKey(%q) accepted", bad)
		}
	}
}

func TestPRRefString(t *testing.T) {
	if got := (PRRef{Repo: "octo/repo", PR: 7}).String(); got != "octo/repo#7" {
		t.Fatalf("PRRef.String = %q", got)
	}
}

func TestParsePRRef(t *testing.T) {
	ref, err := ParsePRRef("octo/repo#7")
	if err != nil {
		t.Fatalf("ParsePRRef: %v", err)
	}
	if ref != (PRRef{Repo: "octo/repo", PR: 7}) {
		t.Fatalf("ref = %+v", ref)
	}
	for _, bad := range []string{"", "octo/repo", "#7", "octo/repo#", "octo/repo#zero", "octo/repo#0", "octo/repo#-1"} {
		if _, err := ParsePRRef(bad); err == nil {
			t.Fatalf("ParsePRRef(%q) accepted", bad)
		}
	}
}
