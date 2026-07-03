package awsx

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestPresenceTouchClearsDisconnect(t *testing.T) {
	fake := &fakeDynamo{}
	store := NewDynamoPresenceStore(fake, "buffer")
	at := time.Unix(1_000, 0)
	if err := store.Touch(context.Background(), testSub, at); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	in := fake.updates[0]
	if sk := in.Key["sk"].(*types.AttributeValueMemberS).Value; sk != skPresence {
		t.Fatalf("sk = %q", sk)
	}
	expr := *in.UpdateExpression
	if !strings.Contains(expr, "SET last_seen") || !strings.Contains(expr, "REMOVE disconnected_at") {
		t.Fatalf("touch expression = %q — must also clear disconnected_at", expr)
	}
	if got := in.ExpressionAttributeValues[":t"].(*types.AttributeValueMemberN).Value; got != "1000" {
		t.Fatalf(":t = %q", got)
	}
}

func TestPresenceMarkDisconnectedKeepsLastSeen(t *testing.T) {
	fake := &fakeDynamo{}
	store := NewDynamoPresenceStore(fake, "buffer")
	if err := store.MarkDisconnected(context.Background(), testSub, time.Unix(2_000, 0)); err != nil {
		t.Fatalf("MarkDisconnected: %v", err)
	}
	expr := *fake.updates[0].UpdateExpression
	if !strings.Contains(expr, "SET disconnected_at") || strings.Contains(expr, "last_seen") {
		t.Fatalf("mark expression = %q — must set only disconnected_at", expr)
	}
}

func TestPresenceStaleScansAndParses(t *testing.T) {
	fake := &fakeDynamo{}
	fake.scanFn = func(in *dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
		if in.ExclusiveStartKey == nil {
			return &dynamodb.ScanOutput{
				Items:            []map[string]types.AttributeValue{{"pk": &types.AttributeValueMemberS{Value: "1#sa"}}},
				LastEvaluatedKey: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "1#sa"}},
			}, nil
		}
		return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{{"pk": &types.AttributeValueMemberS{Value: "2#sb"}}}}, nil
	}
	store := NewDynamoPresenceStore(fake, "buffer")
	stale, err := store.Stale(context.Background(), time.Unix(5_000, 0))
	if err != nil {
		t.Fatalf("Stale: %v", err)
	}
	if len(stale) != 2 || stale[0].UserID != "1" || stale[1].UserID != "2" {
		t.Fatalf("stale = %+v", stale)
	}
	filter := *fake.scans[0].FilterExpression
	// The crash case: a row that still looks connected (no
	// disconnected_at) is stale purely on last_seen.
	if !strings.Contains(filter, "attribute_not_exists(disconnected_at)") {
		t.Fatalf("filter = %q — must treat crash leftovers as stale", filter)
	}
	if got := fake.scans[0].ExpressionAttributeValues[":cutoff"].(*types.AttributeValueMemberN).Value; got != "5000" {
		t.Fatalf(":cutoff = %q", got)
	}
}

func TestPresenceDelete(t *testing.T) {
	fake := &fakeDynamo{}
	store := NewDynamoPresenceStore(fake, "buffer")
	if err := store.Delete(context.Background(), testSub); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if sk := fake.deletes[0].Key["sk"].(*types.AttributeValueMemberS).Value; sk != skPresence {
		t.Fatalf("sk = %q", sk)
	}
}
