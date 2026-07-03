package awsx

import (
	"encoding/base64"
	"io"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

// echoHandler proves the adapter delivers method, path, headers, and body
// faithfully and maps the response back.
func echoHandler(t *testing.T, wantMethod, wantPath, wantBody string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != wantMethod {
			t.Errorf("method = %q, want %q", r.Method, wantMethod)
		}
		if r.URL.Path != wantPath {
			t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if string(body) != wantBody {
			t.Errorf("body = %q, want %q", body, wantBody)
		}
		w.Header().Set("X-Echo", r.Header.Get("X-Github-Event"))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("enqueued"))
	})
}

func functionURLRequest(method, path, body string) events.LambdaFunctionURLRequest {
	req := events.LambdaFunctionURLRequest{
		RawPath: path,
		Body:    body,
		Headers: map[string]string{"X-GitHub-Event": "workflow_run"},
	}
	req.RequestContext.HTTP.Method = method
	return req
}

func TestFunctionURLHandler(t *testing.T) {
	h := FunctionURLHandler(echoHandler(t, http.MethodPost, "/webhook", "payload"))
	res, err := h(t.Context(), functionURLRequest(http.MethodPost, "/webhook", "payload"))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", res.StatusCode)
	}
	if res.Body != "enqueued" {
		t.Fatalf("body = %q", res.Body)
	}
	if res.Headers["X-Echo"] != "workflow_run" {
		t.Fatalf("headers not mapped: %v", res.Headers)
	}
}

func TestFunctionURLHandlerBase64Body(t *testing.T) {
	h := FunctionURLHandler(echoHandler(t, http.MethodPost, "/webhook", "raw bytes"))
	req := functionURLRequest(http.MethodPost, "/webhook", base64.StdEncoding.EncodeToString([]byte("raw bytes")))
	req.IsBase64Encoded = true
	res, err := h(t.Context(), req)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", res.StatusCode)
	}
}

func TestFunctionURLHandlerBadBase64(t *testing.T) {
	h := FunctionURLHandler(http.NotFoundHandler())
	req := functionURLRequest(http.MethodPost, "/webhook", "%%% not base64 %%%")
	req.IsBase64Encoded = true
	if _, err := h(t.Context(), req); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestFunctionURLHandlerDefaults(t *testing.T) {
	// Empty method and path default to GET /.
	h := FunctionURLHandler(echoHandler(t, http.MethodGet, "/", ""))
	req := events.LambdaFunctionURLRequest{}
	res, err := h(t.Context(), req)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", res.StatusCode)
	}
}
