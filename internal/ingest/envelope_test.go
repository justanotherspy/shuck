package ingest

import (
	"strings"
	"testing"
)

func validEnvelope() Envelope {
	return Envelope{
		Schema:         EnvelopeSchema,
		DeliveryID:     "d-1",
		Kind:           KindCIFailure,
		Repo:           "octo/repo",
		PR:             7,
		RunID:          99,
		HeadSHA:        "abc",
		InstallationID: 3,
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	want := validEnvelope()
	data, err := want.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := ParseEnvelope(data)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestEnvelopeValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Envelope)
		wantErr string // substring; empty means valid
	}{
		{"valid ci_failure", func(*Envelope) {}, ""},
		{"valid pr_closed", func(e *Envelope) { e.Kind = KindPRClosed; e.RunID = 0 }, ""},
		{"wrong schema", func(e *Envelope) { e.Schema = 2 }, "schema"},
		{"missing delivery", func(e *Envelope) { e.DeliveryID = "" }, "delivery_id"},
		{"missing repo", func(e *Envelope) { e.Repo = "" }, "repo"},
		{"bad pr", func(e *Envelope) { e.PR = 0 }, "invalid pr"},
		{"ci_failure without run", func(e *Envelope) { e.RunID = 0 }, "run_id"},
		{"valid review_comment", func(e *Envelope) {
			e.Kind = KindReviewComment
			e.CommentID = 9001
			e.AuthorID = 555
		}, ""},
		{"review_comment without comment id", func(e *Envelope) {
			e.Kind = KindReviewComment
			e.AuthorID = 555
		}, "comment_id"},
		{"review_comment without author id", func(e *Envelope) {
			e.Kind = KindReviewComment
			e.CommentID = 9001
		}, "author_id"},
		{"valid review", func(e *Envelope) {
			e.Kind = KindReview
			e.ReviewID = 314
			e.AuthorID = 555
		}, ""},
		{"review without review id", func(e *Envelope) {
			e.Kind = KindReview
			e.AuthorID = 555
		}, "review_id"},
		{"review without author id", func(e *Envelope) {
			e.Kind = KindReview
			e.ReviewID = 314
		}, "author_id"},
		{"unknown kind", func(e *Envelope) { e.Kind = "deployment_status" }, "unknown envelope kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := validEnvelope()
			tc.mutate(&e)
			err := e.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseEnvelopeRejects(t *testing.T) {
	if _, err := ParseEnvelope([]byte("{not json")); err == nil {
		t.Fatal("expected a decode error")
	}
	if _, err := ParseEnvelope([]byte(`{"schema":1}`)); err == nil {
		t.Fatal("expected a validation error")
	}
}
