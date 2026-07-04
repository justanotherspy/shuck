package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// fakeDynamo scripts the narrowed DynamoDB API: per-method Fn fields (nil =
// empty success) plus recorded inputs.
type fakeDynamo struct {
	deleteFn   func(*dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error)
	scanFn     func(*dynamodb.ScanInput) (*dynamodb.ScanOutput, error)
	transactFn func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error)

	deletes   []*dynamodb.DeleteItemInput
	scans     []*dynamodb.ScanInput
	transacts []*dynamodb.TransactWriteItemsInput
}

func (f *fakeDynamo) DeleteItem(_ context.Context, in *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	f.deletes = append(f.deletes, in)
	if f.deleteFn != nil {
		return f.deleteFn(in)
	}
	return &dynamodb.DeleteItemOutput{}, nil
}

func (f *fakeDynamo) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	f.scans = append(f.scans, in)
	if f.scanFn != nil {
		return f.scanFn(in)
	}
	return &dynamodb.ScanOutput{}, nil
}

func (f *fakeDynamo) TransactWriteItems(_ context.Context, in *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	f.transacts = append(f.transacts, in)
	if f.transactFn != nil {
		return f.transactFn(in)
	}
	return &dynamodb.TransactWriteItemsOutput{}, nil
}
