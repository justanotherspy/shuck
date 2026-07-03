package awsx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/justanotherspy/shuck/internal/gateway"
)

func testEvent() gateway.Event {
	return gateway.Event{ID: "ev-1", Repo: "octo/repo", PR: 7, Kind: gateway.KindCIFailure, Summary: "boom"}
}

func counterOutput(n string) *dynamodb.UpdateItemOutput {
	return &dynamodb.UpdateItemOutput{Attributes: map[string]types.AttributeValue{
		"n": &types.AttributeValueMemberN{Value: n},
	}}
}

func TestBufferAppendWritesEventAndMarker(t *testing.T) {
	fake := &fakeDynamo{
		updateFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) { return counterOutput("5"), nil },
	}
	buf := NewDynamoEventBuffer(fake, "buffer", time.Hour)
	fixed := time.Unix(1_000_000, 0)
	buf.now = func() time.Time { return fixed }

	seq, dup, err := buf.Append(context.Background(), testSub, testEvent())
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if seq != 5 || dup {
		t.Fatalf("seq=%d dup=%v, want 5,false", seq, dup)
	}

	// The counter bump.
	up := fake.updates[0]
	if *up.UpdateExpression != "ADD n :one" {
		t.Fatalf("counter update = %q", *up.UpdateExpression)
	}
	if sk := up.Key["sk"].(*types.AttributeValueMemberS).Value; sk != skCounter {
		t.Fatalf("counter sk = %q", sk)
	}

	// One transaction: the event row and the guarded marker.
	tx := fake.transacts[0].TransactItems
	if len(tx) != 2 {
		t.Fatalf("transact items = %d", len(tx))
	}
	eventPut, markerPut := tx[0].Put, tx[1].Put
	if sk := eventPut.Item["sk"].(*types.AttributeValueMemberS).Value; sk != skEvent(5) {
		t.Fatalf("event sk = %q", sk)
	}
	if got := eventPut.Item["expires"].(*types.AttributeValueMemberN).Value; got != "1003600" {
		t.Fatalf("event expires = %q, want now+ttl", got)
	}
	if sk := markerPut.Item["sk"].(*types.AttributeValueMemberS).Value; sk != "e#ev-1" {
		t.Fatalf("marker sk = %q", sk)
	}
	if markerPut.ConditionExpression == nil || *markerPut.ConditionExpression != "attribute_not_exists(pk)" {
		t.Fatal("marker put is not conditional — retries would double-buffer")
	}
	if eventPut.ConditionExpression != nil {
		t.Fatal("event row must not be conditional; the marker guards the pair")
	}
}

func TestBufferAppendDuplicate(t *testing.T) {
	code := "ConditionalCheckFailed"
	fake := &fakeDynamo{
		updateFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) { return counterOutput("6"), nil },
		transactFn: func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
			return nil, &types.TransactionCanceledException{CancellationReasons: []types.CancellationReason{
				{Code: aws.String("None")}, {Code: &code},
			}}
		},
		getFn: func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{
				"seq": &types.AttributeValueMemberN{Value: "3"},
			}}, nil
		},
	}
	buf := NewDynamoEventBuffer(fake, "buffer", time.Hour)
	seq, dup, err := buf.Append(context.Background(), testSub, testEvent())
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !dup || seq != 3 {
		t.Fatalf("seq=%d dup=%v, want the marker's 3,true", seq, dup)
	}
}

func TestBufferAppendOtherTransactErrorPropagates(t *testing.T) {
	fake := &fakeDynamo{
		updateFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) { return counterOutput("1"), nil },
		transactFn: func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
			return nil, errors.New("throttled")
		},
	}
	buf := NewDynamoEventBuffer(fake, "buffer", time.Hour)
	if _, _, err := buf.Append(context.Background(), testSub, testEvent()); err == nil {
		t.Fatal("a non-conditional transact failure must surface (worker retries)")
	}

	// A cancellation without any ConditionalCheckFailed reason is also an
	// operational failure, not a duplicate.
	fake.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		return nil, &types.TransactionCanceledException{CancellationReasons: []types.CancellationReason{
			{Code: aws.String("TransactionConflict")},
		}}
	}
	if _, _, err := buf.Append(context.Background(), testSub, testEvent()); err == nil {
		t.Fatal("a conflict cancellation must not read as a duplicate")
	}
}

func eventItem(seq, id string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"seq":      &types.AttributeValueMemberN{Value: seq},
		"event_id": &types.AttributeValueMemberS{Value: id},
		"kind":     &types.AttributeValueMemberS{Value: "ci_failure"},
		"repo":     &types.AttributeValueMemberS{Value: "octo/repo"},
		"pr":       &types.AttributeValueMemberN{Value: "7"},
		"summary":  &types.AttributeValueMemberS{Value: "boom"},
	}
}

func TestBufferAfterQueriesEventRange(t *testing.T) {
	fake := &fakeDynamo{}
	fake.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		if in.ExclusiveStartKey == nil {
			return &dynamodb.QueryOutput{
				Items:            []map[string]types.AttributeValue{eventItem("3", "ev-3")},
				LastEvaluatedKey: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "x"}},
			}, nil
		}
		return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{eventItem("4", "ev-4")}}, nil
	}
	buf := NewDynamoEventBuffer(fake, "buffer", time.Hour)
	events, err := buf.After(context.Background(), testSub, 2)
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if len(events) != 2 || events[0].Seq != 3 || events[1].Seq != 4 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].ID != "ev-3" || events[0].Kind != gateway.KindCIFailure || events[0].PR != 7 {
		t.Fatalf("event = %+v", events[0])
	}
	// The range starts strictly after the cursor and stays inside the
	// event prefix, excluding counter/marker/presence rows.
	vals := fake.queries[0].ExpressionAttributeValues
	if lo := vals[":lo"].(*types.AttributeValueMemberS).Value; lo != skEvent(3) {
		t.Fatalf("lo = %q, want %q", lo, skEvent(3))
	}
	if hi := vals[":hi"].(*types.AttributeValueMemberS).Value; hi != skEventMax {
		t.Fatalf("hi = %q", hi)
	}
}

func TestBufferAfterRejectsMalformedRows(t *testing.T) {
	for name, item := range map[string]map[string]types.AttributeValue{
		"missing seq":  {"pr": &types.AttributeValueMemberN{Value: "7"}},
		"zero pr":      eventItem("3", "ev-3"),
		"oversized pr": eventItem("3", "ev-3"),
	} {
		switch name {
		case "zero pr":
			item["pr"] = &types.AttributeValueMemberN{Value: "0"}
		case "oversized pr":
			item["pr"] = &types.AttributeValueMemberN{Value: "4294967296"}
		}
		fake := &fakeDynamo{queryFn: func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
			return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{item}}, nil
		}}
		buf := NewDynamoEventBuffer(fake, "buffer", time.Hour)
		if _, err := buf.After(context.Background(), testSub, 0); err == nil {
			t.Fatalf("%s: malformed row accepted", name)
		}
	}
}

func TestBufferSeqOfAndAck(t *testing.T) {
	fake := &fakeDynamo{
		getFn: func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if sk := in.Key["sk"].(*types.AttributeValueMemberS).Value; sk != "e#ev-1" {
				return &dynamodb.GetItemOutput{}, nil
			}
			return &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{
				"seq": &types.AttributeValueMemberN{Value: "9"},
			}}, nil
		},
	}
	buf := NewDynamoEventBuffer(fake, "buffer", time.Hour)

	seq, ok, err := buf.SeqOf(context.Background(), testSub, "ev-1")
	if err != nil || !ok || seq != 9 {
		t.Fatalf("SeqOf = %d,%v,%v", seq, ok, err)
	}
	if _, ok, _ := buf.SeqOf(context.Background(), testSub, "unknown"); ok {
		t.Fatal("unknown id resolved")
	}

	if err := buf.Ack(context.Background(), testSub, "ev-1"); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	del := fake.deletes[0]
	if sk := del.Key["sk"].(*types.AttributeValueMemberS).Value; sk != skEvent(9) {
		t.Fatalf("ack deleted %q, want the event row", sk)
	}

	// Acking an unknown id is a no-op, not an error.
	if err := buf.Ack(context.Background(), testSub, "unknown"); err != nil {
		t.Fatalf("Ack unknown: %v", err)
	}
	if len(fake.deletes) != 1 {
		t.Fatal("ack of an unknown id issued a delete")
	}
}

func TestBufferPurgeDeletesWholePartition(t *testing.T) {
	fake := &fakeDynamo{}
	fake.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{
			subItem("42#sess-1", skCounter),
			subItem("42#sess-1", skEvent(1)),
			subItem("42#sess-1", "e#ev-1"),
			subItem("42#sess-1", skPresence),
		}}, nil
	}
	buf := NewDynamoEventBuffer(fake, "buffer", time.Hour)
	if err := buf.Purge(context.Background(), testSub); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if got := len(fake.batches[0].RequestItems["buffer"]); got != 4 {
		t.Fatalf("purged %d rows, want all 4 shapes", got)
	}
}
