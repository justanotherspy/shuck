package awsx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
)

type fakeDynamo struct {
	putErr    error
	deleteErr error
	puts      []*dynamodb.PutItemInput
	deletes   []*dynamodb.DeleteItemInput
}

func (f *fakeDynamo) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.puts = append(f.puts, in)
	if f.putErr != nil {
		return nil, f.putErr
	}
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDynamo) DeleteItem(_ context.Context, in *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	f.deletes = append(f.deletes, in)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &dynamodb.DeleteItemOutput{}, nil
}

func TestDynamoDeduperFirstSeen(t *testing.T) {
	fake := &fakeDynamo{}
	d := NewDynamoDeduper(fake, "dedupe", time.Hour)
	d.now = func() time.Time { return time.Unix(1_000_000, 0) }

	seen, err := d.Seen(t.Context(), "guid-1")
	if err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if seen {
		t.Fatal("first delivery reported as seen")
	}
	if len(fake.puts) != 1 {
		t.Fatalf("puts = %d, want 1", len(fake.puts))
	}
	in := fake.puts[0]
	if *in.TableName != "dedupe" {
		t.Fatalf("table = %q", *in.TableName)
	}
	if *in.ConditionExpression != "attribute_not_exists(pk)" {
		t.Fatalf("condition = %q", *in.ConditionExpression)
	}
	pk := in.Item["pk"].(*types.AttributeValueMemberS).Value
	if pk != "guid-1" {
		t.Fatalf("pk = %q", pk)
	}
	expires := in.Item["expires"].(*types.AttributeValueMemberN).Value
	if expires != "1003600" { // now + 1h
		t.Fatalf("expires = %q, want 1003600", expires)
	}
}

func TestDynamoDeduperDuplicate(t *testing.T) {
	cases := []struct {
		name   string
		putErr error
	}{
		{"bare conditional failure", &types.ConditionalCheckFailedException{}},
		{
			// The real SDK wraps the API error in an OperationError;
			// duplicate detection must unwrap it.
			"SDK-wrapped conditional failure",
			&smithy.OperationError{
				ServiceID:     "DynamoDB",
				OperationName: "PutItem",
				Err:           &types.ConditionalCheckFailedException{},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDynamo{putErr: tc.putErr}
			d := NewDynamoDeduper(fake, "dedupe", time.Hour)
			seen, err := d.Seen(t.Context(), "guid-1")
			if err != nil {
				t.Fatalf("conditional failure must not be an error: %v", err)
			}
			if !seen {
				t.Fatal("conditional failure means the GUID was already recorded")
			}
		})
	}
}

func TestDynamoDeduperError(t *testing.T) {
	fake := &fakeDynamo{putErr: errors.New("throttled")}
	d := NewDynamoDeduper(fake, "dedupe", time.Hour)
	if _, err := d.Seen(t.Context(), "guid-1"); err == nil {
		t.Fatal("expected the put error to propagate")
	}
}

func TestDynamoDeduperForget(t *testing.T) {
	fake := &fakeDynamo{}
	d := NewDynamoDeduper(fake, "dedupe", time.Hour)
	if err := d.Forget(t.Context(), "guid-1"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if len(fake.deletes) != 1 {
		t.Fatalf("deletes = %d, want 1", len(fake.deletes))
	}
	pk := fake.deletes[0].Key["pk"].(*types.AttributeValueMemberS).Value
	if pk != "guid-1" {
		t.Fatalf("deleted pk = %q", pk)
	}
	fake.deleteErr = errors.New("throttled")
	if err := d.Forget(t.Context(), "guid-2"); err == nil {
		t.Fatal("expected the delete error to propagate")
	}
}
