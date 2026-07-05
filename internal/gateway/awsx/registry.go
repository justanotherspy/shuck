package awsx

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/justanotherspy/shuck/internal/gateway"
	"github.com/justanotherspy/shuck/internal/gateway/serverless"
)

// Registry row shape, in the buffer table alongside the other discriminated
// sort keys: the forward row (pk = subscriber, sk = "w") holds the current
// connection id; the reverse row (pk = "conn#<id>", sk = "w") resolves a
// connection back to its authenticated subscriber. Both carry the TTL so a
// crashed $disconnect only ever leaves garbage that expires.
const (
	skRegistry    = "w"
	connKeyPrefix = "conn#"
)

// DefaultRegistryTTL bounds registry-row retention. API Gateway hard-caps
// WebSocket connections at two hours, so anything older is dead weight.
const DefaultRegistryTTL = 3 * time.Hour

// DynamoRegistryStore implements serverless.RegistryStore on the buffer
// table.
type DynamoRegistryStore struct {
	client DynamoAPI
	table  string
	ttl    time.Duration
	now    func() time.Time
}

// NewDynamoRegistryStore returns a registry store on the buffer table. A
// non-positive ttl means DefaultRegistryTTL.
func NewDynamoRegistryStore(client DynamoAPI, table string, ttl time.Duration) *DynamoRegistryStore {
	if ttl <= 0 {
		ttl = DefaultRegistryTTL
	}
	return &DynamoRegistryStore{client: client, table: table, ttl: ttl, now: time.Now}
}

// Set makes connID the current connection for sub, returning the displaced
// connection id. The forward put is atomic (ReturnValues ALL_OLD), so two
// racing hellos still agree on a single winner.
func (s *DynamoRegistryStore) Set(ctx context.Context, sub gateway.SubscriberKey, connID string) (string, error) {
	expires := strconv.FormatInt(s.now().Add(s.ttl).Unix(), 10)
	out, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"pk":      &types.AttributeValueMemberS{Value: sub.String()},
			"sk":      &types.AttributeValueMemberS{Value: skRegistry},
			"conn":    &types.AttributeValueMemberS{Value: connID},
			"expires": &types.AttributeValueMemberN{Value: expires},
		},
		ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		return "", fmt.Errorf("registry set %s: %w", sub.String(), err)
	}
	if _, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"pk":      &types.AttributeValueMemberS{Value: connKeyPrefix + connID},
			"sk":      &types.AttributeValueMemberS{Value: skRegistry},
			"sub":     &types.AttributeValueMemberS{Value: sub.String()},
			"expires": &types.AttributeValueMemberN{Value: expires},
		},
	}); err != nil {
		return "", fmt.Errorf("registry set reverse %s: %w", connID, err)
	}
	prev := stringAttr(out.Attributes, "conn")
	if prev != "" && prev != connID {
		// Best-effort: a leftover reverse row resolves to the same
		// subscriber until its TTL, and the displaced connection is being
		// closed anyway.
		_, _ = s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(s.table),
			Key:       registryReverseKey(prev),
		})
	}
	return prev, nil
}

// Get returns sub's current connection, if any. The read is consistent —
// a just-replaced connection must not be pushed to.
func (s *DynamoRegistryStore) Get(ctx context.Context, sub gateway.SubscriberKey) (string, bool, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            registryForwardKey(sub),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return "", false, fmt.Errorf("registry get %s: %w", sub.String(), err)
	}
	connID := stringAttr(out.Item, "conn")
	return connID, connID != "", nil
}

// Lookup resolves a connection id back to its subscriber, consistently — a
// frame can arrive on the invocation after the hello that registered it.
func (s *DynamoRegistryStore) Lookup(ctx context.Context, connID string) (gateway.SubscriberKey, bool, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            registryReverseKey(connID),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return gateway.SubscriberKey{}, false, fmt.Errorf("registry lookup %s: %w", connID, err)
	}
	raw := stringAttr(out.Item, "sub")
	if raw == "" {
		return gateway.SubscriberKey{}, false, nil
	}
	sub, err := gateway.ParseSubscriberKey(raw)
	if err != nil {
		return gateway.SubscriberKey{}, false, fmt.Errorf("registry lookup %s: %w", connID, err)
	}
	return sub, true, nil
}

// Remove deletes connID's mapping. The forward row is deleted only while it
// still names connID, so a replaced connection's disconnect never disturbs
// its successor's registration.
func (s *DynamoRegistryStore) Remove(ctx context.Context, connID string) (gateway.SubscriberKey, bool, error) {
	sub, ok, err := s.Lookup(ctx, connID)
	if err != nil || !ok {
		return gateway.SubscriberKey{}, false, err
	}
	if _, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key:       registryReverseKey(connID),
	}); err != nil {
		return gateway.SubscriberKey{}, false, fmt.Errorf("registry remove reverse %s: %w", connID, err)
	}
	_, err = s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:           aws.String(s.table),
		Key:                 registryForwardKey(sub),
		ConditionExpression: aws.String("#c = :conn"),
		ExpressionAttributeNames: map[string]string{
			"#c": "conn",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":conn": &types.AttributeValueMemberS{Value: connID},
		},
	})
	if err != nil {
		var conditionFailed *types.ConditionalCheckFailedException
		if !errors.As(err, &conditionFailed) {
			return gateway.SubscriberKey{}, false, fmt.Errorf("registry remove %s: %w", sub.String(), err)
		}
		// The forward mapping already names a newer connection: leave it.
	}
	return sub, true, nil
}

func registryForwardKey(sub gateway.SubscriberKey) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: sub.String()},
		"sk": &types.AttributeValueMemberS{Value: skRegistry},
	}
}

func registryReverseKey(connID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: connKeyPrefix + connID},
		"sk": &types.AttributeValueMemberS{Value: skRegistry},
	}
}

// The store must satisfy the serverless interface it implements.
var _ serverless.RegistryStore = (*DynamoRegistryStore)(nil)
