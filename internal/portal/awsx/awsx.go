// Package awsx provides the DynamoDB-backed implementation of the portal's
// TokenStore — the writer side of the gateway token table. It is the only
// portal package that imports AWS SDKs; the portal core stays pure so the
// portable shuck CLI never links any of this.
//
// The table shape is the gateway's frozen deployment contract (docs/V2.md):
//
//	tokens  pk (S) = hex sha256 of the bearer token;
//	        attrs github_user_id (N), github_login (S),
//	        repo_allowlist (L, reserved), created (N);
//	        last_used (N) is stamped by the gateway on hello
package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// DynamoAPI is the subset of the DynamoDB client the portal store uses;
// narrowing it keeps the adapter testable without the network.
type DynamoAPI interface {
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	Scan(ctx context.Context, in *dynamodb.ScanInput, opts ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, opts ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}
