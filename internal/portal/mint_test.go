package portal

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/gateway"
)

var tokenShape = regexp.MustCompile(`^shk_[A-Za-z0-9_-]{43}$`)

func TestNewTokenShape(t *testing.T) {
	raw, err := NewToken(nil)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if !tokenShape.MatchString(raw) {
		t.Errorf("token %q does not match shk_ + 43 base64url chars", raw)
	}
	again, err := NewToken(nil)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if raw == again {
		t.Error("two mints returned the same token")
	}
}

func TestNewTokenDeterministic(t *testing.T) {
	r := bytes.NewReader(bytes.Repeat([]byte{0xAB}, tokenBytes))
	raw, err := NewToken(r)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if !strings.HasPrefix(raw, TokenPrefix) {
		t.Errorf("token %q missing prefix", raw)
	}
}

func TestNewTokenShortRandomness(t *testing.T) {
	if _, err := NewToken(bytes.NewReader([]byte{1, 2, 3})); err == nil {
		t.Fatal("exhausted randomness accepted")
	}
}

func TestMintFirstToken(t *testing.T) {
	store := newFakeStore()
	now := time.Unix(1_700_000_000, 0)
	raw, regenerated, err := Mint(context.Background(), store, nil, now, 42, "octocat")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if regenerated {
		t.Error("first mint reported as regenerate")
	}
	hash := gateway.HashToken(raw)
	if !store.has(hash) {
		t.Fatal("stored pk is not gateway.HashToken(raw)")
	}
	row := store.rows[hash]
	if row.GitHubUserID != 42 || row.GitHubLogin != "octocat" || !row.Created.Equal(now) {
		t.Errorf("row = %+v", row)
	}
}

func TestMintRegenerateRevokesAllPrior(t *testing.T) {
	store := newFakeStore()
	store.rows["old-1"] = TokenRow{Hash: "old-1", GitHubUserID: 42}
	store.rows["old-2"] = TokenRow{Hash: "old-2", GitHubUserID: 42} // orphan from a crash
	store.rows["keep"] = TokenRow{Hash: "keep", GitHubUserID: 7}    // someone else's

	raw, regenerated, err := Mint(context.Background(), store, nil, time.Unix(1, 0), 42, "octocat")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !regenerated {
		t.Error("regenerate not reported")
	}
	if store.has("old-1") || store.has("old-2") {
		t.Error("prior tokens survived the regenerate")
	}
	if !store.has("keep") {
		t.Error("another user's token was revoked")
	}
	if !store.has(gateway.HashToken(raw)) {
		t.Error("new token not stored")
	}
	// The revoke+mint went through one atomic Replace, not delete-then-put.
	if len(store.ops) != 1 || !strings.HasPrefix(store.ops[0], "replace:2:") {
		t.Errorf("ops = %v, want one replace of 2 hashes", store.ops)
	}
}

func TestMintStoreFailures(t *testing.T) {
	store := newFakeStore()
	store.listErr = errors.New("scan down")
	if _, _, err := Mint(context.Background(), store, nil, time.Unix(1, 0), 42, "o"); err == nil {
		t.Fatal("list failure not surfaced")
	}
	store = newFakeStore()
	store.writeErr = errors.New("transact down")
	if _, _, err := Mint(context.Background(), store, nil, time.Unix(1, 0), 42, "o"); err == nil {
		t.Fatal("write failure not surfaced")
	}
}
