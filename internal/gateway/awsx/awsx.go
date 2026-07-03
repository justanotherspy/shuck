// Package awsx provides the DynamoDB-backed implementations of the gateway
// store interfaces: the token, subscription, buffer, and presence stores.
// It is the only gateway package that imports AWS SDKs; the gateway core
// stays pure so the portable shuck CLI never links any of this.
//
// Table schemas (the deployment contract, also documented in docs/V2.md):
//
//	tokens         pk (S) = hex sha256 of the bearer token;
//	               attrs github_user_id (N), github_login (S),
//	               repo_allowlist (L, reserved), created (N)
//	subscriptions  pk (S) = "owner/name#pr", sk (S) = "user_id#session_id";
//	               GSI subscriber-index (hash sk, range pk, KEYS_ONLY)
//	buffer         pk (S) = "user_id#session_id"; sk (S) discriminates:
//	               "s#<seq %020d>" event row, "c" seq counter,
//	               "e#<event_id>" dedupe marker, "p" presence;
//	               TTL on the numeric expires attribute
package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// DynamoAPI is the subset of the DynamoDB client the gateway stores use;
// narrowing it keeps the adapters testable without the network.
type DynamoAPI interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Query(ctx context.Context, in *dynamodb.QueryInput, opts ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	Scan(ctx context.Context, in *dynamodb.ScanInput, opts ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	BatchWriteItem(ctx context.Context, in *dynamodb.BatchWriteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
	TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, opts ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}
