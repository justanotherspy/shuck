package awsx

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/justanotherspy/shuck/internal/gateway"
)

var (
	testRef = gateway.PRRef{Repo: "octo/repo", PR: 7}
	testSub = gateway.SubscriberKey{UserID: "42", SessionID: "sess-1"}
)

func subItem(pk, sk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
		"sk": &types.AttributeValueMemberS{Value: sk},
	}
}

func TestSubscribeWritesRow(t *testing.T) {
	fake := &fakeDynamo{}
	store := NewDynamoSubscriptionStore(fake, "subs")
	if err := store.Subscribe(context.Background(), testRef, testSub); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	in := fake.puts[0]
	if *in.TableName != "subs" {
		t.Fatalf("table = %q", *in.TableName)
	}
	if pk := in.Item["pk"].(*types.AttributeValueMemberS).Value; pk != "octo/repo#7" {
		t.Fatalf("pk = %q", pk)
	}
	if sk := in.Item["sk"].(*types.AttributeValueMemberS).Value; sk != "42#sess-1" {
		t.Fatalf("sk = %q", sk)
	}
	if _, ok := in.Item["created"].(*types.AttributeValueMemberN); !ok {
		t.Fatal("created attribute missing")
	}
}

func TestUnsubscribeDeletesRow(t *testing.T) {
	fake := &fakeDynamo{}
	store := NewDynamoSubscriptionStore(fake, "subs")
	if err := store.Unsubscribe(context.Background(), testRef, testSub); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	in := fake.deletes[0]
	if pk := in.Key["pk"].(*types.AttributeValueMemberS).Value; pk != "octo/repo#7" {
		t.Fatalf("pk = %q", pk)
	}
}

func TestSubscribersPaginates(t *testing.T) {
	fake := &fakeDynamo{}
	fake.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		if in.ExclusiveStartKey == nil {
			return &dynamodb.QueryOutput{
				Items:            []map[string]types.AttributeValue{subItem("octo/repo#7", "1#sa")},
				LastEvaluatedKey: subItem("octo/repo#7", "1#sa"),
			}, nil
		}
		return &dynamodb.QueryOutput{
			Items: []map[string]types.AttributeValue{subItem("octo/repo#7", "2#sb")},
		}, nil
	}
	store := NewDynamoSubscriptionStore(fake, "subs")
	subs, err := store.Subscribers(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Subscribers: %v", err)
	}
	if len(subs) != 2 || subs[0].UserID != "1" || subs[1].UserID != "2" {
		t.Fatalf("subs = %+v", subs)
	}
	if len(fake.queries) != 2 {
		t.Fatalf("queries = %d, want 2 (pagination)", len(fake.queries))
	}
}

func TestBySubscriberUsesReverseIndex(t *testing.T) {
	fake := &fakeDynamo{}
	fake.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		if in.IndexName == nil || *in.IndexName != SubscriberIndex {
			return nil, errors.New("expected the subscriber index")
		}
		return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{
			subItem("octo/repo#7", "42#sess-1"),
			subItem("octo/other#9", "42#sess-1"),
		}}, nil
	}
	store := NewDynamoSubscriptionStore(fake, "subs")
	refs, err := store.BySubscriber(context.Background(), testSub)
	if err != nil {
		t.Fatalf("BySubscriber: %v", err)
	}
	if len(refs) != 2 || refs[0] != testRef || refs[1] != (gateway.PRRef{Repo: "octo/other", PR: 9}) {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestRemoveAllForPRBatchDeletes(t *testing.T) {
	fake := &fakeDynamo{}
	fake.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{
			subItem("octo/repo#7", "1#sa"),
			subItem("octo/repo#7", "2#sb"),
		}}, nil
	}
	store := NewDynamoSubscriptionStore(fake, "subs")
	if err := store.RemoveAllForPR(context.Background(), testRef); err != nil {
		t.Fatalf("RemoveAllForPR: %v", err)
	}
	if len(fake.batches) != 1 {
		t.Fatalf("batches = %d", len(fake.batches))
	}
	if got := len(fake.batches[0].RequestItems["subs"]); got != 2 {
		t.Fatalf("delete requests = %d, want 2", got)
	}
}

func TestRemoveAllForSubscriberDeletesEachRef(t *testing.T) {
	fake := &fakeDynamo{}
	fake.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{
			subItem("octo/repo#7", "42#sess-1"),
		}}, nil
	}
	store := NewDynamoSubscriptionStore(fake, "subs")
	if err := store.RemoveAllForSubscriber(context.Background(), testSub); err != nil {
		t.Fatalf("RemoveAllForSubscriber: %v", err)
	}
	reqs := fake.batches[0].RequestItems["subs"]
	if len(reqs) != 1 {
		t.Fatalf("delete requests = %d", len(reqs))
	}
	key := reqs[0].DeleteRequest.Key
	if pk := key["pk"].(*types.AttributeValueMemberS).Value; pk != "octo/repo#7" {
		t.Fatalf("pk = %q", pk)
	}
	if sk := key["sk"].(*types.AttributeValueMemberS).Value; sk != "42#sess-1" {
		t.Fatalf("sk = %q", sk)
	}
}

func TestBatchDeleteRetriesUnprocessedAndChunks(t *testing.T) {
	fake := &fakeDynamo{}
	calls := 0
	fake.batchFn = func(in *dynamodb.BatchWriteItemInput) (*dynamodb.BatchWriteItemOutput, error) {
		calls++
		if calls == 1 {
			// Report the first key unprocessed once.
			return &dynamodb.BatchWriteItemOutput{UnprocessedItems: map[string][]types.WriteRequest{
				"subs": {in.RequestItems["subs"][0]},
			}}, nil
		}
		return &dynamodb.BatchWriteItemOutput{}, nil
	}
	// 30 keys: one full chunk of 25, then 5 + 1 retried.
	var keys []map[string]types.AttributeValue
	for i := range 30 {
		keys = append(keys, subItem("octo/repo#7", "u#"+string(rune('a'+i))))
	}
	if err := batchDelete(context.Background(), fake, "subs", keys); err != nil {
		t.Fatalf("batchDelete: %v", err)
	}
	if calls != 2 {
		t.Fatalf("batch calls = %d, want 2", calls)
	}
	if got := len(fake.batches[0].RequestItems["subs"]); got != batchWriteMax {
		t.Fatalf("first chunk = %d, want %d", got, batchWriteMax)
	}
	if got := len(fake.batches[1].RequestItems["subs"]); got != 6 {
		t.Fatalf("second chunk = %d, want 6 (5 remaining + 1 unprocessed retry)", got)
	}
}

func TestSubscriptionStoreErrorsPropagate(t *testing.T) {
	fake := &fakeDynamo{
		putFn:   func(*dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) { return nil, errors.New("boom") },
		queryFn: func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) { return nil, errors.New("boom") },
	}
	store := NewDynamoSubscriptionStore(fake, "subs")
	if err := store.Subscribe(context.Background(), testRef, testSub); err == nil {
		t.Fatal("Subscribe swallowed the error")
	}
	if _, err := store.Subscribers(context.Background(), testRef); err == nil {
		t.Fatal("Subscribers swallowed the error")
	}
	if _, err := store.BySubscriber(context.Background(), testSub); err == nil {
		t.Fatal("BySubscriber swallowed the error")
	}
}

func TestSubscribersRejectsMalformedRows(t *testing.T) {
	fake := &fakeDynamo{queryFn: func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{
			{"pk": &types.AttributeValueMemberS{Value: "octo/repo#7"}, "sk": &types.AttributeValueMemberS{Value: "no-separator"}},
		}}, nil
	}}
	store := NewDynamoSubscriptionStore(fake, "subs")
	if _, err := store.Subscribers(context.Background(), testRef); err == nil {
		t.Fatal("malformed subscriber key accepted")
	}
}
