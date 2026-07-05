package awsx

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"

	"github.com/aws/aws-lambda-go/events"

	"github.com/justanotherspy/shuck/internal/gateway/serverless"
)

// API Gateway WebSocket event types, as delivered in the request context.
const (
	wsEventConnect    = "CONNECT"
	wsEventMessage    = "MESSAGE"
	wsEventDisconnect = "DISCONNECT"
)

// WSLambdaHandler adapts a serverless.Gateway to the Lambda invocation
// behind an API Gateway WebSocket API's $connect, $default, and $disconnect
// routes — one function serves all three, dispatched on the event type.
// Responses are always 200 except for events missing a connection id (an
// API Gateway contract violation): the core signals verdicts by closing
// connections, not by status code, which API Gateway would only log anyway.
func WSLambdaHandler(g *serverless.Gateway, log *slog.Logger) func(context.Context, events.APIGatewayWebsocketProxyRequest) (events.APIGatewayProxyResponse, error) {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, req events.APIGatewayWebsocketProxyRequest) (events.APIGatewayProxyResponse, error) {
		connID := req.RequestContext.ConnectionID
		if connID == "" {
			return events.APIGatewayProxyResponse{StatusCode: http.StatusBadRequest}, nil
		}
		switch req.RequestContext.EventType {
		case wsEventConnect:
			if err := g.Connect(ctx, connID); err != nil {
				return events.APIGatewayProxyResponse{StatusCode: http.StatusInternalServerError}, nil
			}
		case wsEventDisconnect:
			g.Disconnect(ctx, connID)
		case wsEventMessage:
			body := []byte(req.Body)
			if req.IsBase64Encoded {
				decoded, err := base64.StdEncoding.DecodeString(req.Body)
				if err != nil {
					log.Info("dropping frame: bad base64 body", "conn", connID, "err", err)
					return events.APIGatewayProxyResponse{StatusCode: http.StatusBadRequest}, nil
				}
				body = decoded
			}
			if err := g.Message(ctx, connID, body); err != nil {
				return events.APIGatewayProxyResponse{StatusCode: http.StatusInternalServerError}, nil
			}
		default:
			log.Warn("unknown websocket event type", "type", req.RequestContext.EventType, "conn", connID)
			return events.APIGatewayProxyResponse{StatusCode: http.StatusBadRequest}, nil
		}
		return events.APIGatewayProxyResponse{StatusCode: http.StatusOK}, nil
	}
}

// SweepLambdaHandler runs one function per invocation — the EventBridge
// scheduled entrypoint for the gateway's grace-window sweep.
func SweepLambdaHandler(sweep func(context.Context)) func(context.Context) error {
	return func(ctx context.Context) error {
		sweep(ctx)
		return nil
	}
}
