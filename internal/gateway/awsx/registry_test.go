package awsx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/justanotherspy/shuck/internal/gateway"
)

var regSub = gateway.SubscriberKey{UserID: "42", SessionID: "sess-1"}

func regStore(fake *fakeDynamo) *DynamoRegistryStore {
	s := NewDynamoRegistryStore(fake, "buffer", time.Hour)
	s.now = func() time.Time { return time.Unix(1000, 0) }
	return s
}

func TestRegistrySetWritesBothRows(t *testing.T) {
	fake := &fakeDynamo{}
	prev, err := regStore(fake).Set(context.Background(), regSub, "conn-1")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if prev != "" {
		t.Fatalf("prev = %q, want empty", prev)
	}
	if len(fake.puts) != 2 {
		t.Fatalf("puts = %d, want forward + reverse", len(fake.puts))
	}
	forward := fake.puts[0]
	if pk := forward.Item["pk"].(*types.AttributeValueMemberS).Value; pk != "42#sess-1" {
		t.Fatalf("forward pk = %q", pk)
	}
	if sk := forward.Item["sk"].(*types.AttributeValueMemberS).Value; sk != skRegistry {
		t.Fatalf("forward sk = %q", sk)
	}
	if conn := forward.Item["conn"].(*types.AttributeValueMemberS).Value; conn != "conn-1" {
		t.Fatalf("forward conn = %q", conn)
	}
	if expires := forward.Item["expires"].(*types.AttributeValueMemberN).Value; expires != "4600" { // 1000 + 1h
		t.Fatalf("expires = %q, want 4600", expires)
	}
	if forward.ReturnValues != types.ReturnValueAllOld {
		t.Fatalf("forward ReturnValues = %q, want ALL_OLD", forward.ReturnValues)
	}
	reverse := fake.puts[1]
	if pk := reverse.Item["pk"].(*types.AttributeValueMemberS).Value; pk != "conn#conn-1" {
		t.Fatalf("reverse pk = %q", pk)
	}
	if sub := reverse.Item["sub"].(*types.AttributeValueMemberS).Value; sub != "42#sess-1" {
		t.Fatalf("reverse sub = %q", sub)
	}
}

func TestRegistrySetReturnsAndCleansDisplacedConnection(t *testing.T) {
	fake := &fakeDynamo{
		putFn: func(in *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
			if in.ReturnValues == types.ReturnValueAllOld {
				return &dynamodb.PutItemOutput{Attributes: map[string]types.AttributeValue{
					"conn": &types.AttributeValueMemberS{Value: "conn-old"},
				}}, nil
			}
			return &dynamodb.PutItemOutput{}, nil
		},
	}
	prev, err := regStore(fake).Set(context.Background(), regSub, "conn-new")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if prev != "conn-old" {
		t.Fatalf("prev = %q, want conn-old", prev)
	}
	if len(fake.deletes) != 1 {
		t.Fatalf("deletes = %d, want the old reverse row cleaned", len(fake.deletes))
	}
	if pk := fake.deletes[0].Key["pk"].(*types.AttributeValueMemberS).Value; pk != "conn#conn-old" {
		t.Fatalf("deleted pk = %q", pk)
	}
}

func TestRegistrySetErrors(t *testing.T) {
	fake := &fakeDynamo{putFn: func(*dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
		return nil, errors.New("throttled")
	}}
	if _, err := regStore(fake).Set(context.Background(), regSub, "conn-1"); err == nil {
		t.Fatal("set with failing put returned nil error")
	}
}

func TestRegistryGetConsistentRead(t *testing.T) {
	fake := &fakeDynamo{getFn: func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{
			"conn": &types.AttributeValueMemberS{Value: "conn-1"},
		}}, nil
	}}
	connID, ok, err := regStore(fake).Get(context.Background(), regSub)
	if err != nil || !ok || connID != "conn-1" {
		t.Fatalf("get = %q, %v, %v", connID, ok, err)
	}
	if cr := fake.gets[0].ConsistentRead; cr == nil || !*cr {
		t.Fatal("get did not use a consistent read")
	}
}

func TestRegistryGetMissing(t *testing.T) {
	fake := &fakeDynamo{}
	_, ok, err := regStore(fake).Get(context.Background(), regSub)
	if err != nil || ok {
		t.Fatalf("get on empty = ok %v, err %v; want miss", ok, err)
	}
}

func TestRegistryLookup(t *testing.T) {
	fake := &fakeDynamo{getFn: func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		if pk := in.Key["pk"].(*types.AttributeValueMemberS).Value; pk != "conn#conn-1" {
			t.Fatalf("lookup pk = %q", pk)
		}
		return &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{
			"sub": &types.AttributeValueMemberS{Value: "42#sess-1"},
		}}, nil
	}}
	sub, ok, err := regStore(fake).Lookup(context.Background(), "conn-1")
	if err != nil || !ok || sub != regSub {
		t.Fatalf("lookup = %v, %v, %v", sub, ok, err)
	}
}

func TestRegistryLookupBadSubscriberKey(t *testing.T) {
	fake := &fakeDynamo{getFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{
			"sub": &types.AttributeValueMemberS{Value: "no-separator"},
		}}, nil
	}}
	if _, _, err := regStore(fake).Lookup(context.Background(), "conn-1"); err == nil {
		t.Fatal("lookup with a corrupt subscriber key returned nil error")
	}
}

func TestRegistryRemoveConditionalForwardDelete(t *testing.T) {
	fake := &fakeDynamo{getFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{
			"sub": &types.AttributeValueMemberS{Value: "42#sess-1"},
		}}, nil
	}}
	sub, ok, err := regStore(fake).Remove(context.Background(), "conn-1")
	if err != nil || !ok || sub != regSub {
		t.Fatalf("remove = %v, %v, %v", sub, ok, err)
	}
	if len(fake.deletes) != 2 {
		t.Fatalf("deletes = %d, want reverse + forward", len(fake.deletes))
	}
	forward := fake.deletes[1]
	if forward.ConditionExpression == nil || *forward.ConditionExpression != "#c = :conn" {
		t.Fatal("forward delete is not conditional on the connection id")
	}
	if conn := forward.ExpressionAttributeValues[":conn"].(*types.AttributeValueMemberS).Value; conn != "conn-1" {
		t.Fatalf("condition conn = %q", conn)
	}
}

func TestRegistryRemoveToleratesDisplacedForward(t *testing.T) {
	fake := &fakeDynamo{
		getFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{
				"sub": &types.AttributeValueMemberS{Value: "42#sess-1"},
			}}, nil
		},
		deleteFn: func(in *dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
			if in.ConditionExpression != nil {
				return nil, &types.ConditionalCheckFailedException{}
			}
			return &dynamodb.DeleteItemOutput{}, nil
		},
	}
	sub, ok, err := regStore(fake).Remove(context.Background(), "conn-old")
	if err != nil || !ok || sub != regSub {
		t.Fatalf("remove = %v, %v, %v (a displaced forward row is not an error)", sub, ok, err)
	}
}

func TestRegistryRemoveUnknownConnection(t *testing.T) {
	fake := &fakeDynamo{}
	_, ok, err := regStore(fake).Remove(context.Background(), "never-seen")
	if err != nil || ok {
		t.Fatalf("remove unknown = ok %v, err %v; want miss", ok, err)
	}
	if len(fake.deletes) != 0 {
		t.Fatal("remove of an unknown connection issued deletes")
	}
}
