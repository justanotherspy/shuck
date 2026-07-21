package awsx

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/justanotherspy/shuck/internal/gateway"
)

// DynamoPresenceStore implements gateway.PresenceStore on the buffer
// table's presence rows (sk = "p"). Stale is a filtered scan — the table is
// small and TTL-pruned, so a periodic scan is the v1 cost/simplicity trade;
// a sparse GSI on disconnected_at is the upgrade path if it ever isn't.
type DynamoPresenceStore struct {
	client DynamoAPI
	table  string
}

// NewDynamoPresenceStore returns a presence store on the buffer table.
func NewDynamoPresenceStore(client DynamoAPI, table string) *DynamoPresenceStore {
	return &DynamoPresenceStore{client: client, table: table}
}

// Touch records sub as connected and active at t.
func (s *DynamoPresenceStore) Touch(ctx context.Context, sub gateway.SubscriberKey, at time.Time) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(s.table),
		Key:                       presenceKey(sub),
		UpdateExpression:          aws.String("SET last_seen = :t REMOVE disconnected_at"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":t": &types.AttributeValueMemberN{Value: strconv.FormatInt(at.Unix(), 10)}},
	})
	if err != nil {
		return fmt.Errorf("presence touch %s: %w", sub.String(), err)
	}
	return nil
}

// MarkDisconnected records that sub's connection closed at t. It also
// backfills last_seen when the row doesn't carry one — a row created by a
// disconnect alone (the connect-time Touch failed and the conn died before
// the first TouchInterval) would otherwise never match Stale's
// `last_seen < :cutoff` filter, leaking the subscriber's TTL-less
// subscription rows forever. An existing last_seen is never overwritten.
func (s *DynamoPresenceStore) MarkDisconnected(ctx context.Context, sub gateway.SubscriberKey, at time.Time) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(s.table),
		Key:                       presenceKey(sub),
		UpdateExpression:          aws.String("SET disconnected_at = :t, last_seen = if_not_exists(last_seen, :t)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":t": &types.AttributeValueMemberN{Value: strconv.FormatInt(at.Unix(), 10)}},
	})
	if err != nil {
		return fmt.Errorf("presence mark %s: %w", sub.String(), err)
	}
	return nil
}

// Stale lists subscribers with no touch and no disconnect newer than
// cutoff. A row without disconnected_at but with a stale last_seen counts:
// that is what a crashed gateway leaves behind.
func (s *DynamoPresenceStore) Stale(ctx context.Context, cutoff time.Time) ([]gateway.SubscriberKey, error) {
	var out []gateway.SubscriberKey
	var start map[string]types.AttributeValue
	for {
		page, err := s.client.Scan(ctx, &dynamodb.ScanInput{
			TableName:        aws.String(s.table),
			FilterExpression: aws.String("sk = :p AND last_seen < :cutoff AND (attribute_not_exists(disconnected_at) OR disconnected_at < :cutoff)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":p":      &types.AttributeValueMemberS{Value: skPresence},
				":cutoff": &types.AttributeValueMemberN{Value: strconv.FormatInt(cutoff.Unix(), 10)},
			},
			ProjectionExpression: aws.String("pk"),
			ExclusiveStartKey:    start,
		})
		if err != nil {
			return nil, fmt.Errorf("presence scan: %w", err)
		}
		for _, item := range page.Items {
			key, err := gateway.ParseSubscriberKey(stringAttr(item, "pk"))
			if err != nil {
				return nil, fmt.Errorf("presence scan: %w", err)
			}
			out = append(out, key)
		}
		if page.LastEvaluatedKey == nil {
			return out, nil
		}
		start = page.LastEvaluatedKey
	}
}

// Delete removes sub's presence row.
func (s *DynamoPresenceStore) Delete(ctx context.Context, sub gateway.SubscriberKey) error {
	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key:       presenceKey(sub),
	})
	if err != nil {
		return fmt.Errorf("presence delete %s: %w", sub.String(), err)
	}
	return nil
}

func presenceKey(sub gateway.SubscriberKey) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: sub.String()},
		"sk": &types.AttributeValueMemberS{Value: skPresence},
	}
}
