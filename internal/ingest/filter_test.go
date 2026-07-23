package ingest

import (
	"slices"
	"strings"
	"testing"
)

const workflowRunFailure = `{
	"action": "completed",
	"repository": {"full_name": "octo/repo"},
	"installation": {"id": 77},
	"workflow_run": {
		"id": 1234,
		"conclusion": "failure",
		"head_sha": "abc123",
		"pull_requests": [{"number": 9}, {"number": 10}]
	}
}`

const prClosed = `{
	"action": "closed",
	"number": 42,
	"repository": {"full_name": "octo/repo"},
	"installation": {"id": 77}
}`

const reviewCommentCreated = `{
	"action": "created",
	"repository": {"full_name": "octo/repo"},
	"installation": {"id": 77},
	"pull_request": {"number": 42, "head": {"sha": "abc123"}},
	"comment": {"id": 9001, "user": {"id": 555, "login": "octocat"}}
}`

const reviewSubmitted = `{
	"action": "submitted",
	"repository": {"full_name": "octo/repo"},
	"installation": {"id": 77},
	"pull_request": {"number": 42, "head": {"sha": "abc123"}},
	"review": {"id": 314, "user": {"id": 555, "login": "octocat"}}
}`

func TestFilterWorkflowRunFailure(t *testing.T) {
	// A run associated with several PRs fans out to one envelope per PR —
	// subscription fan-out is keyed repo#pr, so each PR needs its own.
	dec, err := Filter("workflow_run", []byte(workflowRunFailure))
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(dec.Envelopes) == 0 {
		t.Fatalf("expected enqueue, dropped: %s", dec.Reason)
	}
	base := Envelope{
		Schema:         EnvelopeSchema,
		Kind:           KindCIFailure,
		Repo:           "octo/repo",
		RunID:          1234,
		HeadSHA:        "abc123",
		InstallationID: 77,
	}
	for i, pr := range []int{9, 10} {
		want := base
		want.PR = pr
		if dec.Envelopes[i] != want {
			t.Fatalf("envelope[%d] = %+v, want %+v", i, dec.Envelopes[i], want)
		}
	}
	if len(dec.Envelopes) != 2 {
		t.Fatalf("envelopes = %d, want one per associated PR (2)", len(dec.Envelopes))
	}
	// DeliveryID is the handler's job; with it stamped the envelopes are valid.
	for _, env := range dec.Envelopes {
		env.DeliveryID = "guid"
		if err := env.Validate(); err != nil {
			t.Fatalf("stamped envelope invalid: %v", err)
		}
	}
}

func TestFilterWorkflowRunPRFanOut(t *testing.T) {
	cases := []struct {
		name    string
		refs    string // replacement for the fixture's pull_requests array
		wantPRs []int
	}{
		{"two PRs", `[{"number": 9}, {"number": 10}]`, []int{9, 10}},
		{"empty ref mixed with a real one", `[{}, {"number": 10}]`, []int{10}},
		{"duplicate refs collapse", `[{"number": 9}, {"number": 9}]`, []int{9}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(workflowRunFailure, `[{"number": 9}, {"number": 10}]`, tc.refs, 1)
			dec, err := Filter("workflow_run", []byte(body))
			if err != nil {
				t.Fatalf("Filter: %v", err)
			}
			var got []int
			for _, env := range dec.Envelopes {
				got = append(got, env.PR)
			}
			if !slices.Equal(got, tc.wantPRs) {
				t.Fatalf("envelope PRs = %v, want %v", got, tc.wantPRs)
			}
		})
	}
}

func TestFilterPRClosed(t *testing.T) {
	dec, err := Filter("pull_request", []byte(prClosed))
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(dec.Envelopes) != 1 {
		t.Fatalf("envelopes = %d (reason %q), want 1", len(dec.Envelopes), dec.Reason)
	}
	want := Envelope{
		Schema:         EnvelopeSchema,
		Kind:           KindPRClosed,
		Repo:           "octo/repo",
		PR:             42,
		InstallationID: 77,
	}
	if dec.Envelopes[0] != want {
		t.Fatalf("envelope = %+v, want %+v", dec.Envelopes[0], want)
	}
}

func TestFilterReviewComment(t *testing.T) {
	dec, err := Filter("pull_request_review_comment", []byte(reviewCommentCreated))
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(dec.Envelopes) != 1 {
		t.Fatalf("envelopes = %d (reason %q), want 1", len(dec.Envelopes), dec.Reason)
	}
	want := Envelope{
		Schema:         EnvelopeSchema,
		Kind:           KindReviewComment,
		Repo:           "octo/repo",
		PR:             42,
		HeadSHA:        "abc123",
		InstallationID: 77,
		CommentID:      9001,
		AuthorID:       555,
		AuthorLogin:    "octocat",
	}
	if dec.Envelopes[0] != want {
		t.Fatalf("envelope = %+v, want %+v", dec.Envelopes[0], want)
	}
	env := dec.Envelopes[0]
	env.DeliveryID = "guid"
	if err := env.Validate(); err != nil {
		t.Fatalf("stamped envelope invalid: %v", err)
	}
}

func TestFilterReview(t *testing.T) {
	dec, err := Filter("pull_request_review", []byte(reviewSubmitted))
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(dec.Envelopes) != 1 {
		t.Fatalf("envelopes = %d (reason %q), want 1", len(dec.Envelopes), dec.Reason)
	}
	want := Envelope{
		Schema:         EnvelopeSchema,
		Kind:           KindReview,
		Repo:           "octo/repo",
		PR:             42,
		HeadSHA:        "abc123",
		InstallationID: 77,
		ReviewID:       314,
		AuthorID:       555,
		AuthorLogin:    "octocat",
	}
	if dec.Envelopes[0] != want {
		t.Fatalf("envelope = %+v, want %+v", dec.Envelopes[0], want)
	}
	env := dec.Envelopes[0]
	env.DeliveryID = "guid"
	if err := env.Validate(); err != nil {
		t.Fatalf("stamped envelope invalid: %v", err)
	}
}

func TestFilterDrops(t *testing.T) {
	cases := []struct {
		name   string
		event  string
		body   string
		reason string // substring the drop reason must contain
	}{
		{
			"workflow_run success",
			"workflow_run",
			strings.Replace(workflowRunFailure, `"failure"`, `"success"`, 1),
			`conclusion "success"`,
		},
		{
			"workflow_run requested action",
			"workflow_run",
			strings.Replace(workflowRunFailure, `"completed"`, `"requested"`, 1),
			`action "requested"`,
		},
		{
			"workflow_run without PR",
			"workflow_run",
			strings.Replace(workflowRunFailure, `[{"number": 9}, {"number": 10}]`, `[]`, 1),
			"not associated with a pull request",
		},
		{
			"workflow_run with empty PR refs",
			"workflow_run",
			strings.Replace(workflowRunFailure, `[{"number": 9}, {"number": 10}]`, `[{}]`, 1),
			"not associated with a pull request",
		},
		{
			"workflow_run without run id",
			"workflow_run",
			strings.Replace(workflowRunFailure, `"id": 1234,`, ``, 1),
			"no run id",
		},
		{
			"pull_request opened",
			"pull_request",
			strings.Replace(prClosed, `"closed"`, `"opened"`, 1),
			`action "opened"`,
		},
		{
			"pull_request without number",
			"pull_request",
			strings.Replace(prClosed, `"number": 42,`, ``, 1),
			"no number",
		},
		{
			"review comment edited",
			"pull_request_review_comment",
			strings.Replace(reviewCommentCreated, `"created"`, `"edited"`, 1),
			`action "edited"`,
		},
		{
			"review comment without PR number",
			"pull_request_review_comment",
			strings.Replace(reviewCommentCreated, `"number": 42, `, ``, 1),
			"no pull request number",
		},
		{
			"review comment without comment id",
			"pull_request_review_comment",
			strings.Replace(reviewCommentCreated, `"id": 9001, `, ``, 1),
			"no comment id",
		},
		{
			"review comment without author id",
			"pull_request_review_comment",
			strings.Replace(reviewCommentCreated, `"id": 555, `, ``, 1),
			"no author id",
		},
		{
			"review dismissed",
			"pull_request_review",
			strings.Replace(reviewSubmitted, `"submitted"`, `"dismissed"`, 1),
			`action "dismissed"`,
		},
		{
			"review without PR number",
			"pull_request_review",
			strings.Replace(reviewSubmitted, `"number": 42, `, ``, 1),
			"no pull request number",
		},
		{
			"review without review id",
			"pull_request_review",
			strings.Replace(reviewSubmitted, `"id": 314, `, ``, 1),
			"no review id",
		},
		{
			"review without author id",
			"pull_request_review",
			strings.Replace(reviewSubmitted, `"id": 555, `, ``, 1),
			"no author id",
		},
		{
			"unrouted event",
			"star",
			`{"action":"created","repository":{"full_name":"octo/repo"}}`,
			"not routed",
		},
		{
			"missing repository",
			"workflow_run",
			`{"action":"completed","workflow_run":{"id":1,"conclusion":"failure"}}`,
			"no repository",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := Filter(tc.event, []byte(tc.body))
			if err != nil {
				t.Fatalf("Filter: %v", err)
			}
			if len(dec.Envelopes) != 0 {
				t.Fatalf("expected drop, got envelopes %+v", dec.Envelopes)
			}
			if !strings.Contains(dec.Reason, tc.reason) {
				t.Fatalf("reason = %q, want it to contain %q", dec.Reason, tc.reason)
			}
		})
	}
}

func TestFilterCancelledAndTimedOutAreDrillable(t *testing.T) {
	for _, conclusion := range []string{"cancelled", "timed_out"} {
		body := strings.Replace(workflowRunFailure, `"failure"`, `"`+conclusion+`"`, 1)
		dec, err := Filter("workflow_run", []byte(body))
		if err != nil {
			t.Fatalf("Filter(%s): %v", conclusion, err)
		}
		if len(dec.Envelopes) == 0 {
			t.Fatalf("conclusion %q should enqueue, dropped: %s", conclusion, dec.Reason)
		}
	}
}

func TestFilterMalformedPayload(t *testing.T) {
	if _, err := Filter("workflow_run", []byte("{not json")); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}
