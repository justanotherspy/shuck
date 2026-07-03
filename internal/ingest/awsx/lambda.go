package awsx

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// FunctionURLHandler adapts an http.Handler to a Lambda function-URL
// invocation handler, so cmd/shuck-ingest serves the exact same handler in
// both entrypoints (the JUS-86 acceptance requirement). Function URLs use
// the API Gateway v2 payload format.
func FunctionURLHandler(h http.Handler) func(context.Context, events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	return func(ctx context.Context, req events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
		httpReq, err := toHTTPRequest(ctx, req)
		if err != nil {
			return events.LambdaFunctionURLResponse{}, err
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httpReq)

		res := rec.Result()
		defer res.Body.Close()
		headers := make(map[string]string, len(res.Header))
		for k, vs := range res.Header {
			headers[k] = strings.Join(vs, ",")
		}
		return events.LambdaFunctionURLResponse{
			StatusCode: res.StatusCode,
			Headers:    headers,
			Body:       rec.Body.String(),
		}, nil
	}
}

func toHTTPRequest(ctx context.Context, req events.LambdaFunctionURLRequest) (*http.Request, error) {
	body := []byte(req.Body)
	if req.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return nil, fmt.Errorf("decode function URL body: %w", err)
		}
		body = decoded
	}
	path := req.RawPath
	if path == "" {
		path = "/"
	}
	if req.RawQueryString != "" {
		path += "?" + req.RawQueryString
	}
	method := req.RequestContext.HTTP.Method
	if method == "" {
		method = http.MethodGet
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request from function URL event: %w", err)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	return httpReq, nil
}
