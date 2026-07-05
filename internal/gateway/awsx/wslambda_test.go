package awsx

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/justanotherspy/shuck/internal/gateway"
	"github.com/justanotherspy/shuck/internal/gateway/serverless"
)

// wsHarness wires a serverless.Gateway over minimal in-memory fakes for the
// Lambda adapter tests.
type wsHarness struct {
	gateway  *serverless.Gateway
	registry *memRegistryStore
	conns    *memConnAPI
	presence *memPresence
}

func newWSHarness() *wsHarness {
	h := &wsHarness{
		registry: newMemRegistryStore(),
		conns:    newMemConnAPI(),
		presence: newMemPresence(),
	}
	tokens := memTokens{gateway.HashToken("shk_good"): {GitHubUserID: 42, GitHubLogin: "octocat"}}
	h.gateway = &serverless.Gateway{
		Tokens:   tokens,
		Subs:     memSubs{},
		Buffer:   &memBuffer{},
		Presence: h.presence,
		Registry: h.registry,
		Conns:    h.conns,
	}
	return h
}

func wsEvent(eventType, connID, body string) events.APIGatewayWebsocketProxyRequest {
	return events.APIGatewayWebsocketProxyRequest{
		Body: body,
		RequestContext: events.APIGatewayWebsocketProxyRequestContext{
			EventType:    eventType,
			ConnectionID: connID,
		},
	}
}

func TestWSLambdaHandlerLifecycle(t *testing.T) {
	h := newWSHarness()
	handler := WSLambdaHandler(h.gateway, nil)
	ctx := context.Background()

	res, err := handler(ctx, wsEvent("CONNECT", "conn-1", ""))
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("connect = %d, %v", res.StatusCode, err)
	}

	res, err = handler(ctx, wsEvent("MESSAGE", "conn-1", `{"type":"hello","token":"shk_good","session_id":"sess-1"}`))
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("hello = %d, %v", res.StatusCode, err)
	}
	sub, ok, _ := h.registry.Lookup(ctx, "conn-1")
	if !ok || sub.UserID != "42" {
		t.Fatalf("hello did not register: %v %v", sub, ok)
	}

	res, err = handler(ctx, wsEvent("DISCONNECT", "conn-1", ""))
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("disconnect = %d, %v", res.StatusCode, err)
	}
	if _, ok, _ := h.registry.Lookup(ctx, "conn-1"); ok {
		t.Fatal("disconnect left the registry mapping")
	}
}

func TestWSLambdaHandlerBase64Body(t *testing.T) {
	h := newWSHarness()
	handler := WSLambdaHandler(h.gateway, nil)
	body := base64.StdEncoding.EncodeToString([]byte(`{"type":"hello","token":"shk_good","session_id":"sess-1"}`))
	req := wsEvent("MESSAGE", "conn-1", body)
	req.IsBase64Encoded = true

	res, err := handler(context.Background(), req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("hello = %d, %v", res.StatusCode, err)
	}
	if _, ok, _ := h.registry.Lookup(context.Background(), "conn-1"); !ok {
		t.Fatal("base64 hello did not register")
	}
}

func TestWSLambdaHandlerBadBase64(t *testing.T) {
	h := newWSHarness()
	handler := WSLambdaHandler(h.gateway, nil)
	req := wsEvent("MESSAGE", "conn-1", "%%% not base64")
	req.IsBase64Encoded = true

	res, err := handler(context.Background(), req)
	if err != nil || res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad base64 = %d, %v; want 400", res.StatusCode, err)
	}
}

func TestWSLambdaHandlerMissingConnectionID(t *testing.T) {
	h := newWSHarness()
	handler := WSLambdaHandler(h.gateway, nil)
	res, err := handler(context.Background(), wsEvent("MESSAGE", "", "{}"))
	if err != nil || res.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing conn id = %d, %v; want 400", res.StatusCode, err)
	}
}

func TestWSLambdaHandlerUnknownEventType(t *testing.T) {
	h := newWSHarness()
	handler := WSLambdaHandler(h.gateway, nil)
	res, err := handler(context.Background(), wsEvent("SOMETHING", "conn-1", ""))
	if err != nil || res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown type = %d, %v; want 400", res.StatusCode, err)
	}
}

func TestSweepLambdaHandler(t *testing.T) {
	ran := 0
	handler := SweepLambdaHandler(func(context.Context) { ran++ })
	if err := handler(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if ran != 1 {
		t.Fatalf("sweep ran %d times, want 1", ran)
	}
}
