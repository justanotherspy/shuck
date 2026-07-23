package ingest

// The ingest Handler serves two entrypoints from the same code: a plain HTTP
// server and a Lambda function URL via lambdahttp. This file drives the real
// handler through lambdahttp.FunctionURLHandler end-to-end, covering the
// function-URL quirks the plain-HTTP tests cannot: base64-encoded bodies
// (which must be decoded before HMAC verification) and lowercase header keys
// (API Gateway v2 lowercases them).

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/justanotherspy/shuck/internal/lambdahttp"
)

func TestHandlerThroughLambdaFunctionURL(t *testing.T) {
	sig := Sign([]byte(testSecret), []byte(workflowRunFailure))
	cases := []struct {
		name        string
		base64Body  bool
		headers     map[string]string
		wantStatus  int
		wantEnqueue int
	}{
		{
			name: "plain body",
			headers: map[string]string{
				SignatureHeader:     sig,
				"X-GitHub-Event":    "workflow_run",
				"X-GitHub-Delivery": "lambda-d-1",
			},
			wantStatus:  http.StatusAccepted,
			wantEnqueue: 2, // the fixture run touches PRs 9 and 10
		},
		{
			name:       "base64 body with lowercase headers",
			base64Body: true,
			headers: map[string]string{
				"x-hub-signature-256": sig,
				"x-github-event":      "workflow_run",
				"x-github-delivery":   "lambda-d-2",
			},
			wantStatus:  http.StatusAccepted,
			wantEnqueue: 2,
		},
		{
			name: "tampered signature",
			headers: map[string]string{
				SignatureHeader:     Sign([]byte("wrong secret"), []byte(workflowRunFailure)),
				"X-GitHub-Event":    "workflow_run",
				"X-GitHub-Delivery": "lambda-d-3",
			},
			wantStatus:  http.StatusUnauthorized,
			wantEnqueue: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQueue{}
			fn := lambdahttp.FunctionURLHandler(newHandler(&fakeDedupe{}, q, nil))

			req := events.LambdaFunctionURLRequest{
				RawPath: "/webhook",
				Body:    workflowRunFailure,
				Headers: tc.headers,
			}
			req.RequestContext.HTTP.Method = http.MethodPost
			if tc.base64Body {
				req.Body = base64.StdEncoding.EncodeToString([]byte(workflowRunFailure))
				req.IsBase64Encoded = true
			}

			res, err := fn(context.Background(), req)
			if err != nil {
				t.Fatalf("FunctionURLHandler: %v", err)
			}
			if res.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", res.StatusCode, tc.wantStatus, res.Body)
			}
			if len(q.got) != tc.wantEnqueue {
				t.Fatalf("enqueued %d envelopes, want %d", len(q.got), tc.wantEnqueue)
			}
			for _, env := range q.got {
				if err := env.Validate(); err != nil {
					t.Fatalf("enqueued envelope invalid: %v (%+v)", err, env)
				}
			}
		})
	}
}
