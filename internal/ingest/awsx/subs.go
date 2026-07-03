package awsx

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// DynamoQueryAPI is the subset of the DynamoDB client the subscription
// checker uses.
type DynamoQueryAPI interface {
	Query(ctx context.Context, in *dynamodb.QueryInput, opts ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

// DynamoSubscriptionChecker implements ingest.SubscriptionChecker against
// the gateway's subscription table (JUS-88: pk = "owner/name#pr"). It is
// the cheap pre-filter that keeps unsubscribed repos out of the queue; the
// handler fails open on errors, so it can never drop a subscribed event.
type DynamoSubscriptionChecker struct {
	client DynamoQueryAPI
	table  string
}

// NewDynamoSubscriptionChecker returns a checker reading the gateway's
// subscription table.
func NewDynamoSubscriptionChecker(client DynamoQueryAPI, table string) *DynamoSubscriptionChecker {
	return &DynamoSubscriptionChecker{client: client, table: table}
}

// HasSubscriber reports whether at least one subscriber exists for repo#pr.
func (c *DynamoSubscriptionChecker) HasSubscriber(ctx context.Context, repo string, pr int) (bool, error) {
	out, err := c.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(c.table),
		KeyConditionExpression: aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: fmt.Sprintf("%s#%d", repo, pr)},
		},
		Limit:                aws.Int32(1),
		ProjectionExpression: aws.String("pk"),
	})
	if err != nil {
		return false, fmt.Errorf("subscription query %s#%d: %w", repo, pr, err)
	}
	return len(out.Items) > 0, nil
}
