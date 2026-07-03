package ingest

import (
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

func TestFilterWorkflowRunFailure(t *testing.T) {
	dec, err := Filter("workflow_run", []byte(workflowRunFailure))
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if !dec.Enqueue {
		t.Fatalf("expected enqueue, dropped: %s", dec.Reason)
	}
	env := dec.Envelope
	want := Envelope{
		Schema:         EnvelopeSchema,
		Kind:           KindCIFailure,
		Repo:           "octo/repo",
		PR:             9,
		RunID:          1234,
		HeadSHA:        "abc123",
		InstallationID: 77,
	}
	if env != want {
		t.Fatalf("envelope = %+v, want %+v", env, want)
	}
	// DeliveryID is the handler's job; with it stamped the envelope is valid.
	env.DeliveryID = "guid"
	if err := env.Validate(); err != nil {
		t.Fatalf("stamped envelope invalid: %v", err)
	}
}

func TestFilterPRClosed(t *testing.T) {
	dec, err := Filter("pull_request", []byte(prClosed))
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if !dec.Enqueue {
		t.Fatalf("expected enqueue, dropped: %s", dec.Reason)
	}
	want := Envelope{
		Schema:         EnvelopeSchema,
		Kind:           KindPRClosed,
		Repo:           "octo/repo",
		PR:             42,
		InstallationID: 77,
	}
	if dec.Envelope != want {
		t.Fatalf("envelope = %+v, want %+v", dec.Envelope, want)
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
			if dec.Enqueue {
				t.Fatalf("expected drop, got envelope %+v", dec.Envelope)
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
		if !dec.Enqueue {
			t.Fatalf("conclusion %q should enqueue, dropped: %s", conclusion, dec.Reason)
		}
	}
}

func TestFilterMalformedPayload(t *testing.T) {
	if _, err := Filter("workflow_run", []byte("{not json")); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}
