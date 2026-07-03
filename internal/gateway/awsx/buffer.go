package awsx

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/justanotherspy/shuck/internal/gateway"
)

// Buffer-table sort keys. Events sort under a zero-padded seq so range
// queries return them in order while the counter, dedupe markers, and
// presence share the subscriber's partition.
const (
	skCounter     = "c"
	skPresence    = "p"
	skEventPrefix = "s#"
	skMarkPrefix  = "e#"
	// skEventMax sorts after every event row (seq is padded to 20 digits).
	skEventMax = "s#99999999999999999999"
)

// skEvent formats an event row's sort key.
func skEvent(seq int64) string {
	return fmt.Sprintf("%s%020d", skEventPrefix, seq)
}

// DynamoEventBuffer implements gateway.EventBuffer on the buffer table.
// Append is write-then-push's "write": a seq from the atomic counter row,
// then one transaction putting the event row and an event_id dedupe marker
// guarded by attribute_not_exists — so worker retries buffer exactly once.
type DynamoEventBuffer struct {
	client DynamoAPI
	table  string
	ttl    time.Duration
	now    func() time.Time
}

// NewDynamoEventBuffer returns a buffer on table whose event and marker
// rows expire after ttl.
func NewDynamoEventBuffer(client DynamoAPI, table string, ttl time.Duration) *DynamoEventBuffer {
	return &DynamoEventBuffer{client: client, table: table, ttl: ttl, now: time.Now}
}

// Append persists ev for sub with the next seq, deduping on ev.ID.
func (b *DynamoEventBuffer) Append(ctx context.Context, sub gateway.SubscriberKey, ev gateway.Event) (seq int64, duplicate bool, err error) {
	seq, err = b.nextSeq(ctx, sub)
	if err != nil {
		return 0, false, fmt.Errorf("buffer seq %s: %w", sub.String(), err)
	}
	// A crash between the counter bump and the transaction leaves a seq
	// gap, which is harmless: replay needs monotonic, not dense.
	expires := strconv.FormatInt(b.now().Add(b.ttl).Unix(), 10)
	pk := &types.AttributeValueMemberS{Value: sub.String()}
	_, err = b.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{
				TableName: aws.String(b.table),
				Item: map[string]types.AttributeValue{
					"pk":       pk,
					"sk":       &types.AttributeValueMemberS{Value: skEvent(seq)},
					"seq":      &types.AttributeValueMemberN{Value: strconv.FormatInt(seq, 10)},
					"event_id": &types.AttributeValueMemberS{Value: ev.ID},
					"kind":     &types.AttributeValueMemberS{Value: string(ev.Kind)},
					"repo":     &types.AttributeValueMemberS{Value: ev.Repo},
					"pr":       &types.AttributeValueMemberN{Value: strconv.Itoa(ev.PR)},
					"summary":  &types.AttributeValueMemberS{Value: ev.Summary},
					"expires":  &types.AttributeValueMemberN{Value: expires},
				},
			}},
			{Put: &types.Put{
				TableName: aws.String(b.table),
				Item: map[string]types.AttributeValue{
					"pk":      pk,
					"sk":      &types.AttributeValueMemberS{Value: skMarkPrefix + ev.ID},
					"seq":     &types.AttributeValueMemberN{Value: strconv.FormatInt(seq, 10)},
					"expires": &types.AttributeValueMemberN{Value: expires},
				},
				ConditionExpression: aws.String("attribute_not_exists(pk)"),
			}},
		},
	})
	if err != nil {
		if !isConditionalCancel(err) {
			return 0, false, fmt.Errorf("buffer append %s %s: %w", sub.String(), ev.ID, err)
		}
		// The marker exists: this event_id was already buffered for sub.
		existing, ok, err := b.SeqOf(ctx, sub, ev.ID)
		if err != nil {
			return 0, false, fmt.Errorf("buffer dedupe %s %s: %w", sub.String(), ev.ID, err)
		}
		if !ok {
			// Marker raced its own TTL expiry; treat as duplicate anyway
			// — the retry that observed it already delivered.
			return 0, true, nil
		}
		return existing, true, nil
	}
	return seq, false, nil
}

// nextSeq atomically increments the subscriber's counter row.
func (b *DynamoEventBuffer) nextSeq(ctx context.Context, sub gateway.SubscriberKey) (int64, error) {
	out, err := b.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(b.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: sub.String()},
			"sk": &types.AttributeValueMemberS{Value: skCounter},
		},
		UpdateExpression:          aws.String("ADD n :one"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":one": &types.AttributeValueMemberN{Value: "1"}},
		ReturnValues:              types.ReturnValueUpdatedNew,
	})
	if err != nil {
		return 0, err
	}
	return numberAttr(out.Attributes, "n")
}

// After returns sub's buffered events with seq > afterSeq, ascending.
func (b *DynamoEventBuffer) After(ctx context.Context, sub gateway.SubscriberKey, afterSeq int64) ([]gateway.Event, error) {
	var out []gateway.Event
	var start map[string]types.AttributeValue
	for {
		page, err := b.client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(b.table),
			KeyConditionExpression: aws.String("pk = :pk AND sk BETWEEN :lo AND :hi"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": &types.AttributeValueMemberS{Value: sub.String()},
				":lo": &types.AttributeValueMemberS{Value: skEvent(afterSeq + 1)},
				":hi": &types.AttributeValueMemberS{Value: skEventMax},
			},
			ExclusiveStartKey: start,
		})
		if err != nil {
			return nil, fmt.Errorf("buffer query %s: %w", sub.String(), err)
		}
		for _, item := range page.Items {
			ev, err := eventFromItem(item)
			if err != nil {
				return nil, fmt.Errorf("buffer row %s: %w", sub.String(), err)
			}
			out = append(out, ev)
		}
		if page.LastEvaluatedKey == nil {
			return out, nil
		}
		start = page.LastEvaluatedKey
	}
}

// SeqOf resolves an event id to its seq via the dedupe marker.
func (b *DynamoEventBuffer) SeqOf(ctx context.Context, sub gateway.SubscriberKey, eventID string) (seq int64, ok bool, err error) {
	out, err := b.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(b.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: sub.String()},
			"sk": &types.AttributeValueMemberS{Value: skMarkPrefix + eventID},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return 0, false, fmt.Errorf("buffer marker %s %s: %w", sub.String(), eventID, err)
	}
	if out.Item == nil {
		return 0, false, nil
	}
	seq, err = numberAttr(out.Item, "seq")
	if err != nil {
		return 0, false, fmt.Errorf("buffer marker %s %s: %w", sub.String(), eventID, err)
	}
	return seq, true, nil
}

// Ack deletes the acked event row. The dedupe marker stays until its TTL so
// a late worker retry cannot resurrect an acked event.
func (b *DynamoEventBuffer) Ack(ctx context.Context, sub gateway.SubscriberKey, eventID string) error {
	seq, ok, err := b.SeqOf(ctx, sub, eventID)
	if err != nil {
		return err
	}
	if !ok {
		return nil // unknown or expired: nothing to delete
	}
	_, err = b.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(b.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: sub.String()},
			"sk": &types.AttributeValueMemberS{Value: skEvent(seq)},
		},
	})
	if err != nil {
		return fmt.Errorf("buffer ack %s %s: %w", sub.String(), eventID, err)
	}
	return nil
}

// Purge removes every row in sub's partition: events, markers, the
// counter, and the presence row.
func (b *DynamoEventBuffer) Purge(ctx context.Context, sub gateway.SubscriberKey) error {
	var keys []map[string]types.AttributeValue
	var start map[string]types.AttributeValue
	for {
		page, err := b.client.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String(b.table),
			KeyConditionExpression:    aws.String("pk = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: sub.String()}},
			ProjectionExpression:      aws.String("pk, sk"),
			ExclusiveStartKey:         start,
		})
		if err != nil {
			return fmt.Errorf("buffer purge %s: %w", sub.String(), err)
		}
		for _, item := range page.Items {
			keys = append(keys, map[string]types.AttributeValue{"pk": item["pk"], "sk": item["sk"]})
		}
		if page.LastEvaluatedKey == nil {
			break
		}
		start = page.LastEvaluatedKey
	}
	if err := batchDelete(ctx, b.client, b.table, keys); err != nil {
		return fmt.Errorf("buffer purge %s: %w", sub.String(), err)
	}
	return nil
}

// eventFromItem decodes a buffer event row.
func eventFromItem(item map[string]types.AttributeValue) (gateway.Event, error) {
	seq, err := numberAttr(item, "seq")
	if err != nil {
		return gateway.Event{}, err
	}
	pr, err := numberAttr(item, "pr")
	if err != nil {
		return gateway.Event{}, err
	}
	if pr <= 0 || pr > math.MaxInt32 {
		return gateway.Event{}, fmt.Errorf("invalid pr %d", pr)
	}
	return gateway.Event{
		ID:      stringAttr(item, "event_id"),
		Seq:     seq,
		Repo:    stringAttr(item, "repo"),
		PR:      int(pr),
		Kind:    gateway.EventKind(stringAttr(item, "kind")),
		Summary: stringAttr(item, "summary"),
	}, nil
}

// isConditionalCancel reports whether a TransactWriteItems error was a
// cancellation caused by the dedupe marker's condition check.
func isConditionalCancel(err error) bool {
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) {
		return false
	}
	for _, reason := range canceled.CancellationReasons {
		if reason.Code != nil && *reason.Code == "ConditionalCheckFailed" {
			return true
		}
	}
	return false
}
