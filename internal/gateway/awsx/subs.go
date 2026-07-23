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

// SubscriberIndex is the subscription table's reverse GSI (hash sk, range
// pk, KEYS_ONLY) used to list one subscriber's PRs.
const SubscriberIndex = "subscriber-index"

// batchWriteMax is DynamoDB's BatchWriteItem request cap.
const batchWriteMax = 25

// DynamoSubscriptionStore implements gateway.SubscriptionStore on the
// subscription table.
type DynamoSubscriptionStore struct {
	client DynamoAPI
	table  string
	now    func() time.Time
}

// NewDynamoSubscriptionStore returns a subscription store on table.
func NewDynamoSubscriptionStore(client DynamoAPI, table string) *DynamoSubscriptionStore {
	return &DynamoSubscriptionStore{client: client, table: table, now: time.Now}
}

// Subscribe records sub's interest in ref. Re-subscribing overwrites the
// row, which is the desired idempotency.
func (s *DynamoSubscriptionStore) Subscribe(ctx context.Context, ref gateway.PRRef, sub gateway.SubscriberKey) error {
	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"pk":      &types.AttributeValueMemberS{Value: ref.String()},
			"sk":      &types.AttributeValueMemberS{Value: sub.String()},
			"created": &types.AttributeValueMemberN{Value: strconv.FormatInt(s.now().Unix(), 10)},
		},
	})
	if err != nil {
		return fmt.Errorf("subscribe %s %s: %w", ref.String(), sub.String(), err)
	}
	return nil
}

// Unsubscribe removes one subscription row.
func (s *DynamoSubscriptionStore) Unsubscribe(ctx context.Context, ref gateway.PRRef, sub gateway.SubscriberKey) error {
	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: ref.String()},
			"sk": &types.AttributeValueMemberS{Value: sub.String()},
		},
	})
	if err != nil {
		return fmt.Errorf("unsubscribe %s %s: %w", ref.String(), sub.String(), err)
	}
	return nil
}

// Subscribers lists every subscriber of ref for deliver fan-out.
func (s *DynamoSubscriptionStore) Subscribers(ctx context.Context, ref gateway.PRRef) ([]gateway.SubscriberKey, error) {
	var out []gateway.SubscriberKey
	err := s.queryPR(ctx, ref, func(item map[string]types.AttributeValue) error {
		key, err := gateway.ParseSubscriberKey(stringAttr(item, "sk"))
		if err != nil {
			return err
		}
		out = append(out, key)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("subscribers %s: %w", ref.String(), err)
	}
	return out, nil
}

// BySubscriber lists sub's PRs via the reverse index.
func (s *DynamoSubscriptionStore) BySubscriber(ctx context.Context, sub gateway.SubscriberKey) ([]gateway.PRRef, error) {
	var out []gateway.PRRef
	var start map[string]types.AttributeValue
	for {
		page, err := s.client.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String(s.table),
			IndexName:                 aws.String(SubscriberIndex),
			KeyConditionExpression:    aws.String("sk = :sk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":sk": &types.AttributeValueMemberS{Value: sub.String()}},
			ExclusiveStartKey:         start,
		})
		if err != nil {
			return nil, fmt.Errorf("subscriptions of %s: %w", sub.String(), err)
		}
		for _, item := range page.Items {
			ref, err := gateway.ParsePRRef(stringAttr(item, "pk"))
			if err != nil {
				return nil, fmt.Errorf("subscriptions of %s: %w", sub.String(), err)
			}
			out = append(out, ref)
		}
		if page.LastEvaluatedKey == nil {
			return out, nil
		}
		start = page.LastEvaluatedKey
	}
}

// RemoveAllForPR drops every subscription for ref (PR closed or merged).
func (s *DynamoSubscriptionStore) RemoveAllForPR(ctx context.Context, ref gateway.PRRef) error {
	var keys []map[string]types.AttributeValue
	err := s.queryPR(ctx, ref, func(item map[string]types.AttributeValue) error {
		keys = append(keys, map[string]types.AttributeValue{"pk": item["pk"], "sk": item["sk"]})
		return nil
	})
	if err != nil {
		return fmt.Errorf("remove subscriptions %s: %w", ref.String(), err)
	}
	if err := batchDelete(ctx, s.client, s.table, keys); err != nil {
		return fmt.Errorf("remove subscriptions %s: %w", ref.String(), err)
	}
	return nil
}

// RemoveAllForSubscriber drops every subscription held by sub (sweep).
func (s *DynamoSubscriptionStore) RemoveAllForSubscriber(ctx context.Context, sub gateway.SubscriberKey) error {
	refs, err := s.BySubscriber(ctx, sub)
	if err != nil {
		return err
	}
	keys := make([]map[string]types.AttributeValue, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: ref.String()},
			"sk": &types.AttributeValueMemberS{Value: sub.String()},
		})
	}
	if err := batchDelete(ctx, s.client, s.table, keys); err != nil {
		return fmt.Errorf("remove subscriptions of %s: %w", sub.String(), err)
	}
	return nil
}

// queryPR pages through ref's partition, calling visit per row.
func (s *DynamoSubscriptionStore) queryPR(ctx context.Context, ref gateway.PRRef, visit func(map[string]types.AttributeValue) error) error {
	var start map[string]types.AttributeValue
	for {
		page, err := s.client.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String(s.table),
			KeyConditionExpression:    aws.String("pk = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: ref.String()}},
			ExclusiveStartKey:         start,
		})
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			if err := visit(item); err != nil {
				return err
			}
		}
		if page.LastEvaluatedKey == nil {
			return nil
		}
		start = page.LastEvaluatedKey
	}
}

// batchRetryMax caps how many times batchDelete retries unprocessed items
// before giving up with an error.
const batchRetryMax = 5

// batchRetryBase is the first unprocessed-item backoff, doubling per retry.
// A var so tests can shrink it.
var batchRetryBase = 50 * time.Millisecond

// batchDelete deletes keys from table in BatchWriteItem chunks, retrying
// unprocessed keys. Unprocessed items are DynamoDB's throttle signal, so
// each retry backs off (doubling, ctx-cancellable) instead of amplifying the
// throttle in a tight loop, and a bounded retry budget surfaces a persistent
// throttle as an error rather than spinning.
func batchDelete(ctx context.Context, client DynamoAPI, table string, keys []map[string]types.AttributeValue) error {
	retries := 0
	backoff := batchRetryBase
	for len(keys) > 0 {
		n := min(len(keys), batchWriteMax)
		chunk := keys[:n]
		keys = keys[n:]
		requests := make([]types.WriteRequest, 0, len(chunk))
		for _, key := range chunk {
			requests = append(requests, types.WriteRequest{DeleteRequest: &types.DeleteRequest{Key: key}})
		}
		out, err := client.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]types.WriteRequest{table: requests},
		})
		if err != nil {
			return err
		}
		unprocessed := out.UnprocessedItems[table]
		if len(unprocessed) == 0 {
			continue
		}
		if retries >= batchRetryMax {
			return fmt.Errorf("batch delete: %d items still unprocessed after %d retries", len(unprocessed), retries)
		}
		retries++
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		for _, req := range unprocessed {
			if req.DeleteRequest != nil {
				keys = append(keys, req.DeleteRequest.Key)
			}
		}
	}
	return nil
}
