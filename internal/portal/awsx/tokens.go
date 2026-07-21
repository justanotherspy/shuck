package awsx

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/justanotherspy/shuck/internal/portal"
)

// DynamoTokenStore implements portal.TokenStore on the gateway token table.
// Scans are consistent reads: a regenerate must see the row it is about to
// revoke, and the sweep must not resurrect decisions from a stale snapshot.
type DynamoTokenStore struct {
	client DynamoAPI
	table  string
	// Log may be nil, which means slog.Default().
	Log *slog.Logger
}

// NewDynamoTokenStore returns a token store writing to table.
func NewDynamoTokenStore(client DynamoAPI, table string) *DynamoTokenStore {
	return &DynamoTokenStore{client: client, table: table}
}

func (s *DynamoTokenStore) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// ByUser lists the rows owned by userID. The table holds one row per user
// (a small population of operators), so a filtered scan is the simplest
// correct read — the frozen schema has no user-id index.
func (s *DynamoTokenStore) ByUser(ctx context.Context, userID int64) ([]portal.TokenRow, error) {
	return s.scan(ctx, aws.String("github_user_id = :uid"), map[string]types.AttributeValue{
		":uid": &types.AttributeValueMemberN{Value: strconv.FormatInt(userID, 10)},
	})
}

// All lists every row, for the sweep.
func (s *DynamoTokenStore) All(ctx context.Context) ([]portal.TokenRow, error) {
	return s.scan(ctx, nil, nil)
}

func (s *DynamoTokenStore) scan(ctx context.Context, filter *string, values map[string]types.AttributeValue) ([]portal.TokenRow, error) {
	var rows []portal.TokenRow
	var start map[string]types.AttributeValue
	for {
		out, err := s.client.Scan(ctx, &dynamodb.ScanInput{
			TableName:                 aws.String(s.table),
			ConsistentRead:            aws.Bool(true),
			FilterExpression:          filter,
			ExpressionAttributeValues: values,
			ExclusiveStartKey:         start,
		})
		if err != nil {
			return nil, fmt.Errorf("token scan: %w", err)
		}
		for _, item := range out.Items {
			row, err := rowFromItem(item)
			if err != nil {
				// One malformed row (e.g. hand-seeded without the numeric
				// user id) must not abort the whole listing — that would 502
				// every dashboard and permanently stall the sweep. Skip it
				// loudly instead.
				s.log().Warn("skipping malformed token row",
					"pk", stringAttr(item, "pk"), "err", err)
				continue
			}
			rows = append(rows, row)
		}
		if len(out.LastEvaluatedKey) == 0 {
			return rows, nil
		}
		start = out.LastEvaluatedKey
	}
}

// Replace atomically deletes the given hashes and writes row in one
// transaction — the revoke-old + mint-new step. An empty deleteHashes is a
// plain first mint.
func (s *DynamoTokenStore) Replace(ctx context.Context, deleteHashes []string, row portal.TokenRow) error {
	items := make([]types.TransactWriteItem, 0, len(deleteHashes)+1)
	for _, hash := range deleteHashes {
		items = append(items, types.TransactWriteItem{Delete: &types.Delete{
			TableName: aws.String(s.table),
			Key:       map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: hash}},
		}})
	}
	items = append(items, types.TransactWriteItem{Put: &types.Put{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"pk":             &types.AttributeValueMemberS{Value: row.Hash},
			"github_user_id": &types.AttributeValueMemberN{Value: strconv.FormatInt(row.GitHubUserID, 10)},
			"github_login":   &types.AttributeValueMemberS{Value: row.GitHubLogin},
			"repo_allowlist": &types.AttributeValueMemberL{Value: []types.AttributeValue{}},
			"created":        &types.AttributeValueMemberN{Value: strconv.FormatInt(row.Created.Unix(), 10)},
		},
	}})
	if _, err := s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: items}); err != nil {
		return fmt.Errorf("token replace: %w", err)
	}
	return nil
}

// Delete revokes one token; the gateway rejects it at the next hello.
// Deleting a missing row succeeds (DynamoDB delete is idempotent).
func (s *DynamoTokenStore) Delete(ctx context.Context, hash string) error {
	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key:       map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: hash}},
	})
	if err != nil {
		return fmt.Errorf("token delete: %w", err)
	}
	return nil
}

func rowFromItem(item map[string]types.AttributeValue) (portal.TokenRow, error) {
	hash, ok := item["pk"].(*types.AttributeValueMemberS)
	if !ok {
		return portal.TokenRow{}, fmt.Errorf("missing string attribute pk")
	}
	userID, err := numberAttr(item, "github_user_id")
	if err != nil {
		return portal.TokenRow{}, err
	}
	row := portal.TokenRow{
		Hash:         hash.Value,
		GitHubUserID: userID,
		GitHubLogin:  stringAttr(item, "github_login"),
	}
	if created, err := numberAttr(item, "created"); err == nil {
		row.Created = time.Unix(created, 0)
	}
	if used, err := numberAttr(item, "last_used"); err == nil {
		row.LastUsed = time.Unix(used, 0)
	}
	return row, nil
}

// stringAttr reads an optional string attribute.
func stringAttr(item map[string]types.AttributeValue, name string) string {
	if v, ok := item[name].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

// numberAttr reads a numeric attribute.
func numberAttr(item map[string]types.AttributeValue, name string) (int64, error) {
	v, ok := item[name].(*types.AttributeValueMemberN)
	if !ok {
		return 0, fmt.Errorf("missing numeric attribute %s", name)
	}
	n, err := strconv.ParseInt(v.Value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("attribute %s: %w", name, err)
	}
	return n, nil
}
