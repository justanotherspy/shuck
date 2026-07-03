package gateway

import (
	"strings"
	"testing"
)

func validDeliver() DeliverRequest {
	return DeliverRequest{
		Schema:  DeliverSchema,
		EventID: "ev-1",
		Kind:    KindCIFailure,
		Repo:    "octo/repo",
		PR:      7,
		Summary: "lint failed on step 3",
		Author:  &Author{GitHubUserID: 42, Login: "octocat"},
	}
}

func TestDeliverRequestRoundTrip(t *testing.T) {
	want := validDeliver()
	data, err := want.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := ParseDeliverRequest(data)
	if err != nil {
		t.Fatalf("ParseDeliverRequest: %v", err)
	}
	if got.EventID != want.EventID || got.Kind != want.Kind || got.Repo != want.Repo ||
		got.PR != want.PR || got.Summary != want.Summary || *got.Author != *want.Author {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestDeliverRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*DeliverRequest)
		wantErr string // substring; empty means valid
	}{
		{"valid ci_failure", func(*DeliverRequest) {}, ""},
		{"valid pr_closed", func(r *DeliverRequest) { r.Kind = KindPRClosed }, ""},
		{"valid review_comment", func(r *DeliverRequest) { r.Kind = KindReviewComment }, ""},
		{"valid review", func(r *DeliverRequest) { r.Kind = KindReview }, ""},
		{"valid without author", func(r *DeliverRequest) { r.Author = nil }, ""},
		{"wrong schema", func(r *DeliverRequest) { r.Schema = 2 }, "schema"},
		{"missing event_id", func(r *DeliverRequest) { r.EventID = "" }, "event_id"},
		{"missing repo", func(r *DeliverRequest) { r.Repo = "" }, "repo"},
		{"bad pr", func(r *DeliverRequest) { r.PR = 0 }, "invalid pr"},
		{"unknown kind", func(r *DeliverRequest) { r.Kind = "push" }, "unknown deliver kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validDeliver()
			tc.mutate(&r)
			err := r.Validate()
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

func TestParseDeliverRequestRejects(t *testing.T) {
	if _, err := ParseDeliverRequest([]byte("{not json")); err == nil {
		t.Fatal("expected a decode error")
	}
	if _, err := ParseDeliverRequest([]byte(`{"schema":1}`)); err == nil {
		t.Fatal("expected a validation error")
	}
}

func TestSuppressed(t *testing.T) {
	author := &Author{GitHubUserID: 42, Login: "octocat"}
	cases := []struct {
		name   string
		kind   EventKind
		author *Author
		userID string
		want   bool
	}{
		{"review_comment self", KindReviewComment, author, "42", true},
		{"review self", KindReview, author, "42", true},
		{"review_comment other user", KindReviewComment, author, "7", false},
		{"ci_failure never suppressed", KindCIFailure, author, "42", false},
		{"pr_closed never suppressed", KindPRClosed, author, "42", false},
		{"no author", KindReviewComment, nil, "42", false},
		{"zero author id", KindReviewComment, &Author{Login: "octocat"}, "0", false},
		{"login coincidence is not identity", KindReview, &Author{GitHubUserID: 99, Login: "octocat"}, "42", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validDeliver()
			r.Kind = tc.kind
			r.Author = tc.author
			if got := r.Suppressed(tc.userID); got != tc.want {
				t.Fatalf("Suppressed(%q) = %v, want %v", tc.userID, got, tc.want)
			}
		})
	}
}

func TestDeliverRequestEvent(t *testing.T) {
	r := validDeliver()
	ev := r.Event()
	if ev.Type != frameEvent || ev.ID != r.EventID || ev.Repo != r.Repo ||
		ev.PR != r.PR || ev.Kind != r.Kind || ev.Summary != r.Summary {
		t.Fatalf("Event() = %+v does not mirror %+v", ev, r)
	}
	if ev.Seq != 0 {
		t.Fatalf("Event() stamped seq %d; the buffer owns seq", ev.Seq)
	}
}
