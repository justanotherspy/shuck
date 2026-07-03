package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// fakeDynamo is a scriptable DynamoAPI: each method delegates to its
// function field (nil means an empty success) and records inputs.
type fakeDynamo struct {
	getFn      func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error)
	putFn      func(*dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error)
	deleteFn   func(*dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error)
	updateFn   func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error)
	queryFn    func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error)
	scanFn     func(*dynamodb.ScanInput) (*dynamodb.ScanOutput, error)
	batchFn    func(*dynamodb.BatchWriteItemInput) (*dynamodb.BatchWriteItemOutput, error)
	transactFn func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error)

	gets      []*dynamodb.GetItemInput
	puts      []*dynamodb.PutItemInput
	deletes   []*dynamodb.DeleteItemInput
	updates   []*dynamodb.UpdateItemInput
	queries   []*dynamodb.QueryInput
	scans     []*dynamodb.ScanInput
	batches   []*dynamodb.BatchWriteItemInput
	transacts []*dynamodb.TransactWriteItemsInput
}

func (f *fakeDynamo) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.gets = append(f.gets, in)
	if f.getFn != nil {
		return f.getFn(in)
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (f *fakeDynamo) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.puts = append(f.puts, in)
	if f.putFn != nil {
		return f.putFn(in)
	}
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDynamo) DeleteItem(_ context.Context, in *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	f.deletes = append(f.deletes, in)
	if f.deleteFn != nil {
		return f.deleteFn(in)
	}
	return &dynamodb.DeleteItemOutput{}, nil
}

func (f *fakeDynamo) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.updates = append(f.updates, in)
	if f.updateFn != nil {
		return f.updateFn(in)
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func (f *fakeDynamo) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.queries = append(f.queries, in)
	if f.queryFn != nil {
		return f.queryFn(in)
	}
	return &dynamodb.QueryOutput{}, nil
}

func (f *fakeDynamo) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	f.scans = append(f.scans, in)
	if f.scanFn != nil {
		return f.scanFn(in)
	}
	return &dynamodb.ScanOutput{}, nil
}

func (f *fakeDynamo) BatchWriteItem(_ context.Context, in *dynamodb.BatchWriteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error) {
	f.batches = append(f.batches, in)
	if f.batchFn != nil {
		return f.batchFn(in)
	}
	return &dynamodb.BatchWriteItemOutput{}, nil
}

func (f *fakeDynamo) TransactWriteItems(_ context.Context, in *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	f.transacts = append(f.transacts, in)
	if f.transactFn != nil {
		return f.transactFn(in)
	}
	return &dynamodb.TransactWriteItemsOutput{}, nil
}
