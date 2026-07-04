package awsx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/justanotherspy/shuck/internal/portal"
)

func item(hash, userID, login, created string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk":             &types.AttributeValueMemberS{Value: hash},
		"github_user_id": &types.AttributeValueMemberN{Value: userID},
		"github_login":   &types.AttributeValueMemberS{Value: login},
		"created":        &types.AttributeValueMemberN{Value: created},
	}
}

func TestByUser(t *testing.T) {
	fake := &fakeDynamo{scanFn: func(in *dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
		return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{
			item("h1", "42", "octocat", "1700000000"),
		}}, nil
	}}
	store := NewDynamoTokenStore(fake, "tokens")
	rows, err := store.ByUser(context.Background(), 42)
	if err != nil {
		t.Fatalf("ByUser: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	row := rows[0]
	if row.Hash != "h1" || row.GitHubUserID != 42 || row.GitHubLogin != "octocat" ||
		!row.Created.Equal(time.Unix(1_700_000_000, 0)) || !row.LastUsed.IsZero() {
		t.Errorf("row = %+v", row)
	}
	in := fake.scans[0]
	if *in.TableName != "tokens" {
		t.Errorf("table = %q", *in.TableName)
	}
	if in.ConsistentRead == nil || !*in.ConsistentRead {
		t.Error("portal scans must be consistent (regenerate must see its own revocations)")
	}
	if *in.FilterExpression != "github_user_id = :uid" {
		t.Errorf("filter = %q", *in.FilterExpression)
	}
	if got := in.ExpressionAttributeValues[":uid"].(*types.AttributeValueMemberN).Value; got != "42" {
		t.Errorf(":uid = %q", got)
	}
}

func TestAllPaginates(t *testing.T) {
	fake := &fakeDynamo{scanFn: func(in *dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
		if in.ExclusiveStartKey == nil {
			return &dynamodb.ScanOutput{
				Items:            []map[string]types.AttributeValue{item("h1", "1", "a", "1")},
				LastEvaluatedKey: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "h1"}},
			}, nil
		}
		return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{item("h2", "2", "b", "2")}}, nil
	}}
	store := NewDynamoTokenStore(fake, "tokens")
	rows, err := store.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(rows) != 2 || rows[0].Hash != "h1" || rows[1].Hash != "h2" {
		t.Fatalf("rows = %+v", rows)
	}
	if len(fake.scans) != 2 {
		t.Fatalf("scans = %d, want 2 (pagination)", len(fake.scans))
	}
	if fake.scans[0].FilterExpression != nil {
		t.Error("All must not filter")
	}
}

func TestLastUsedRead(t *testing.T) {
	withUsed := item("h1", "42", "octocat", "1700000000")
	withUsed["last_used"] = &types.AttributeValueMemberN{Value: "1700005000"}
	fake := &fakeDynamo{scanFn: func(*dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
		return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{withUsed}}, nil
	}}
	store := NewDynamoTokenStore(fake, "tokens")
	rows, err := store.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if !rows[0].LastUsed.Equal(time.Unix(1_700_005_000, 0)) {
		t.Errorf("last used = %v", rows[0].LastUsed)
	}
}

func TestReplaceTransactShape(t *testing.T) {
	fake := &fakeDynamo{}
	store := NewDynamoTokenStore(fake, "tokens")
	row := portal.TokenRow{
		Hash:         "new-hash",
		GitHubUserID: 42,
		GitHubLogin:  "octocat",
		Created:      time.Unix(1_700_000_000, 0),
	}
	if err := store.Replace(context.Background(), []string{"old-1", "old-2"}, row); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if len(fake.transacts) != 1 {
		t.Fatalf("transacts = %d, want one atomic call", len(fake.transacts))
	}
	items := fake.transacts[0].TransactItems
	if len(items) != 3 {
		t.Fatalf("transact items = %d, want 2 deletes + 1 put", len(items))
	}
	for i, hash := range []string{"old-1", "old-2"} {
		del := items[i].Delete
		if del == nil || *del.TableName != "tokens" ||
			del.Key["pk"].(*types.AttributeValueMemberS).Value != hash {
			t.Errorf("item %d = %+v, want delete of %q", i, items[i], hash)
		}
	}
	put := items[2].Put
	if put == nil || *put.TableName != "tokens" {
		t.Fatalf("last item is not the put: %+v", items[2])
	}
	// The frozen JUS-88 row shape, attribute for attribute.
	if got := put.Item["pk"].(*types.AttributeValueMemberS).Value; got != "new-hash" {
		t.Errorf("pk = %q", got)
	}
	if got := put.Item["github_user_id"].(*types.AttributeValueMemberN).Value; got != "42" {
		t.Errorf("github_user_id = %q", got)
	}
	if got := put.Item["github_login"].(*types.AttributeValueMemberS).Value; got != "octocat" {
		t.Errorf("github_login = %q", got)
	}
	if got := put.Item["created"].(*types.AttributeValueMemberN).Value; got != "1700000000" {
		t.Errorf("created = %q", got)
	}
	if list, ok := put.Item["repo_allowlist"].(*types.AttributeValueMemberL); !ok || len(list.Value) != 0 {
		t.Errorf("repo_allowlist = %+v, want empty reserved list", put.Item["repo_allowlist"])
	}
	if _, present := put.Item["last_used"]; present {
		t.Error("mint must not pre-stamp last_used (gateway owns it)")
	}
}

func TestReplaceFirstMint(t *testing.T) {
	fake := &fakeDynamo{}
	store := NewDynamoTokenStore(fake, "tokens")
	if err := store.Replace(context.Background(), nil, portal.TokenRow{Hash: "h", Created: time.Unix(1, 0)}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if items := fake.transacts[0].TransactItems; len(items) != 1 || items[0].Put == nil {
		t.Fatalf("first mint items = %+v, want a single put", items)
	}
}

func TestReplaceError(t *testing.T) {
	fake := &fakeDynamo{transactFn: func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		return nil, errors.New("transact canceled")
	}}
	store := NewDynamoTokenStore(fake, "tokens")
	if err := store.Replace(context.Background(), []string{"old"}, portal.TokenRow{Hash: "h"}); err == nil {
		t.Fatal("transact failure not surfaced")
	}
}

func TestDelete(t *testing.T) {
	fake := &fakeDynamo{}
	store := NewDynamoTokenStore(fake, "tokens")
	if err := store.Delete(context.Background(), "h1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	in := fake.deletes[0]
	if *in.TableName != "tokens" || in.Key["pk"].(*types.AttributeValueMemberS).Value != "h1" {
		t.Errorf("delete input = %+v", in)
	}
}

func TestDeleteError(t *testing.T) {
	fake := &fakeDynamo{deleteFn: func(*dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
		return nil, errors.New("throttled")
	}}
	store := NewDynamoTokenStore(fake, "tokens")
	if err := store.Delete(context.Background(), "h1"); err == nil {
		t.Fatal("delete failure not surfaced")
	}
}

func TestScanErrors(t *testing.T) {
	fake := &fakeDynamo{scanFn: func(*dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
		return nil, errors.New("throttled")
	}}
	store := NewDynamoTokenStore(fake, "tokens")
	if _, err := store.All(context.Background()); err == nil {
		t.Fatal("scan failure not surfaced")
	}

	// A row without the numeric user id is corrupt, not a token.
	fake = &fakeDynamo{scanFn: func(*dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
		return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{
			{"pk": &types.AttributeValueMemberS{Value: "h"}},
		}}, nil
	}}
	store = NewDynamoTokenStore(fake, "tokens")
	if _, err := store.All(context.Background()); err == nil {
		t.Fatal("malformed row accepted")
	}
}
