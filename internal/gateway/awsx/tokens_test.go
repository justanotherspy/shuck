package awsx

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/justanotherspy/shuck/internal/gateway"
)

func TestTokenLookup(t *testing.T) {
	fake := &fakeDynamo{
		getFn: func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{
				"pk":             in.Key["pk"],
				"github_user_id": &types.AttributeValueMemberN{Value: "42"},
				"github_login":   &types.AttributeValueMemberS{Value: "octocat"},
			}}, nil
		},
	}
	store := NewDynamoTokenStore(fake, "tokens")
	rec, err := store.Lookup(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if rec.GitHubUserID != 42 || rec.GitHubLogin != "octocat" {
		t.Fatalf("record = %+v", rec)
	}
	in := fake.gets[0]
	if *in.TableName != "tokens" {
		t.Fatalf("table = %q", *in.TableName)
	}
	if got := in.Key["pk"].(*types.AttributeValueMemberS).Value; got != "abc123" {
		t.Fatalf("pk = %q", got)
	}
	if in.ConsistentRead == nil || !*in.ConsistentRead {
		t.Fatal("token lookups must be consistent reads (revocation latency)")
	}
}

func TestTokenLookupNotFound(t *testing.T) {
	store := NewDynamoTokenStore(&fakeDynamo{}, "tokens")
	_, err := store.Lookup(context.Background(), "missing")
	if !errors.Is(err, gateway.ErrTokenNotFound) {
		t.Fatalf("err = %v, want ErrTokenNotFound", err)
	}
}

func TestTokenLookupErrors(t *testing.T) {
	fake := &fakeDynamo{getFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return nil, errors.New("throttled")
	}}
	store := NewDynamoTokenStore(fake, "tokens")
	if _, err := store.Lookup(context.Background(), "abc"); err == nil || errors.Is(err, gateway.ErrTokenNotFound) {
		t.Fatalf("store failure must not read as not-found: %v", err)
	}

	// A row without the numeric user id is corrupt, not a token.
	fake = &fakeDynamo{getFn: func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{"pk": in.Key["pk"]}}, nil
	}}
	store = NewDynamoTokenStore(fake, "tokens")
	if _, err := store.Lookup(context.Background(), "abc"); err == nil {
		t.Fatal("malformed row accepted")
	}
}
