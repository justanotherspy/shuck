// Package awsx provides the AWS-backed implementations of the ingest
// interfaces — a DynamoDB deduper and an SQS enqueuer — plus the Lambda
// function-URL adapter used by cmd/shuck-ingest. It is the only ingest
// package that imports AWS SDKs; the ingest core stays pure so the portable
// shuck CLI never links any of this.
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
)

// DynamoAPI is the subset of the DynamoDB client the deduper uses;
// narrowing it keeps the adapter testable without the network.
type DynamoAPI interface {
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

// DynamoDeduper implements ingest.Deduper on a DynamoDB table with a string
// partition key `pk` and TTL enabled on the numeric `expires` attribute.
type DynamoDeduper struct {
	client DynamoAPI
	table  string
	ttl    time.Duration
	now    func() time.Time
}

// NewDynamoDeduper returns a deduper writing delivery GUIDs to table with
// the given retention. GitHub redeliveries happen within minutes, so an
// hour of retention is plenty; the TTL keeps the table from growing.
func NewDynamoDeduper(client DynamoAPI, table string, ttl time.Duration) *DynamoDeduper {
	return &DynamoDeduper{client: client, table: table, ttl: ttl, now: time.Now}
}

// Seen conditionally records id and reports whether it was already there.
func (d *DynamoDeduper) Seen(ctx context.Context, id string) (bool, error) {
	expires := d.now().Add(d.ttl).Unix()
	_, err := d.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.table),
		Item: map[string]types.AttributeValue{
			"pk":      &types.AttributeValueMemberS{Value: id},
			"expires": &types.AttributeValueMemberN{Value: strconv.FormatInt(expires, 10)},
		},
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	})
	if err != nil {
		var conditional *types.ConditionalCheckFailedException
		if errors.As(err, &conditional) {
			return true, nil
		}
		return false, fmt.Errorf("dedupe put %s: %w", id, err)
	}
	return false, nil
}

// Forget removes id so a redelivery can be processed after a failed
// enqueue.
func (d *DynamoDeduper) Forget(ctx context.Context, id string) error {
	_, err := d.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(d.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: id},
		},
	})
	if err != nil {
		return fmt.Errorf("dedupe delete %s: %w", id, err)
	}
	return nil
}
