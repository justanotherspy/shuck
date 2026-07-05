package awsx

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apigatewaymanagementapi"
	"github.com/aws/aws-sdk-go-v2/service/apigatewaymanagementapi/types"

	"github.com/justanotherspy/shuck/internal/gateway/serverless"
)

// ManagementAPI is the subset of the API Gateway @connections client the
// gateway uses; narrowing it keeps the adapter testable without the network.
type ManagementAPI interface {
	PostToConnection(ctx context.Context, in *apigatewaymanagementapi.PostToConnectionInput, opts ...func(*apigatewaymanagementapi.Options)) (*apigatewaymanagementapi.PostToConnectionOutput, error)
	DeleteConnection(ctx context.Context, in *apigatewaymanagementapi.DeleteConnectionInput, opts ...func(*apigatewaymanagementapi.Options)) (*apigatewaymanagementapi.DeleteConnectionOutput, error)
}

// APIGWConnAPI implements serverless.ConnAPI on the API Gateway
// @connections management API.
type APIGWConnAPI struct {
	client ManagementAPI
}

// NewAPIGWConnAPI wraps an apigatewaymanagementapi client (constructed with
// the WebSocket API's callback endpoint as its base URL).
func NewAPIGWConnAPI(client ManagementAPI) *APIGWConnAPI {
	return &APIGWConnAPI{client: client}
}

// Post writes one frame to a connection, mapping GoneException to
// serverless.ErrGone.
func (a *APIGWConnAPI) Post(ctx context.Context, connID string, data []byte) error {
	_, err := a.client.PostToConnection(ctx, &apigatewaymanagementapi.PostToConnectionInput{
		ConnectionId: aws.String(connID),
		Data:         data,
	})
	if err != nil {
		var gone *types.GoneException
		if errors.As(err, &gone) {
			return fmt.Errorf("post to %s: %w", connID, serverless.ErrGone)
		}
		return fmt.Errorf("post to %s: %w", connID, err)
	}
	return nil
}

// Close drops a connection. An already-gone connection is a success.
func (a *APIGWConnAPI) Close(ctx context.Context, connID string) error {
	_, err := a.client.DeleteConnection(ctx, &apigatewaymanagementapi.DeleteConnectionInput{
		ConnectionId: aws.String(connID),
	})
	if err != nil {
		var gone *types.GoneException
		if errors.As(err, &gone) {
			return nil
		}
		return fmt.Errorf("close %s: %w", connID, err)
	}
	return nil
}

var _ serverless.ConnAPI = (*APIGWConnAPI)(nil)
