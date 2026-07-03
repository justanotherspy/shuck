package awsx

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type fakeQuery struct {
	out *dynamodb.QueryOutput
	err error
	got *dynamodb.QueryInput
}

func (f *fakeQuery) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.got = in
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

func TestHasSubscriber(t *testing.T) {
	fake := &fakeQuery{out: &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{
		{"pk": &types.AttributeValueMemberS{Value: "octo/repo#7"}},
	}}}
	checker := NewDynamoSubscriptionChecker(fake, "subs")
	ok, err := checker.HasSubscriber(context.Background(), "octo/repo", 7)
	if err != nil || !ok {
		t.Fatalf("HasSubscriber = %v, %v", ok, err)
	}
	if *fake.got.TableName != "subs" {
		t.Fatalf("table = %q", *fake.got.TableName)
	}
	if pk := fake.got.ExpressionAttributeValues[":pk"].(*types.AttributeValueMemberS).Value; pk != "octo/repo#7" {
		t.Fatalf("pk = %q", pk)
	}
	if fake.got.Limit == nil || *fake.got.Limit != 1 {
		t.Fatal("existence probe must use Limit 1")
	}
}

func TestHasSubscriberEmpty(t *testing.T) {
	checker := NewDynamoSubscriptionChecker(&fakeQuery{out: &dynamodb.QueryOutput{}}, "subs")
	ok, err := checker.HasSubscriber(context.Background(), "octo/repo", 7)
	if err != nil || ok {
		t.Fatalf("HasSubscriber = %v, %v; want false on an empty partition", ok, err)
	}
}

func TestHasSubscriberError(t *testing.T) {
	checker := NewDynamoSubscriptionChecker(&fakeQuery{err: errors.New("throttled")}, "subs")
	if _, err := checker.HasSubscriber(context.Background(), "octo/repo", 7); err == nil {
		t.Fatal("store failure swallowed — the handler decides fail-open, not the checker")
	}
}
