package awsx

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/justanotherspy/shuck/internal/gateway"
)

// DynamoTokenStore implements gateway.TokenStore on the token table written
// by the token portal (JUS-90). Revocation is row deletion, so a
// consistent read keeps a revoked token's window as small as possible.
type DynamoTokenStore struct {
	client DynamoAPI
	table  string
}

// NewDynamoTokenStore returns a token store reading from table.
func NewDynamoTokenStore(client DynamoAPI, table string) *DynamoTokenStore {
	return &DynamoTokenStore{client: client, table: table}
}

// Lookup resolves a token hash to its record, or gateway.ErrTokenNotFound.
func (s *DynamoTokenStore) Lookup(ctx context.Context, tokenHash string) (gateway.TokenRecord, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: tokenHash}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return gateway.TokenRecord{}, fmt.Errorf("token get: %w", err)
	}
	if out.Item == nil {
		return gateway.TokenRecord{}, gateway.ErrTokenNotFound
	}
	userID, err := numberAttr(out.Item, "github_user_id")
	if err != nil {
		return gateway.TokenRecord{}, fmt.Errorf("token row: %w", err)
	}
	return gateway.TokenRecord{
		GitHubUserID: userID,
		GitHubLogin:  stringAttr(out.Item, "github_login"),
	}, nil
}

// TouchToken implements gateway.TokenToucher: stamp the row's last_used
// epoch. The attribute_exists condition keeps a touch racing a revocation
// from resurrecting the deleted row as a stub; that race is swallowed as a
// no-op.
func (s *DynamoTokenStore) TouchToken(ctx context.Context, tokenHash string, at time.Time) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(s.table),
		Key:                 map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: tokenHash}},
		UpdateExpression:    aws.String("SET last_used = :t"),
		ConditionExpression: aws.String("attribute_exists(pk)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":t": &types.AttributeValueMemberN{Value: strconv.FormatInt(at.Unix(), 10)},
		},
	})
	var conditional *types.ConditionalCheckFailedException
	if errors.As(err, &conditional) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("token touch: %w", err)
	}
	return nil
}

// stringAttr reads an optional string attribute.
func stringAttr(item map[string]types.AttributeValue, name string) string {
	if v, ok := item[name].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

// numberAttr reads a required numeric attribute.
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
