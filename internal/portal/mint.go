package portal

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"time"

	"github.com/justanotherspy/shuck/internal/gateway"
)

// TokenPrefix marks every minted Shuck token. A recognizable prefix makes
// leaked tokens greppable and secret-scanner friendly without weakening
// them.
const TokenPrefix = "shk_"

// tokenBytes is the entropy behind each token: 32 random bytes (256 bits).
const tokenBytes = 32

// TokenRow is one row of the gateway token table as the portal sees it. The
// row shape is frozen (docs/V2.md § JUS-88 table schemas); LastUsed reads
// the additive last_used attribute the gateway stamps on hello.
type TokenRow struct {
	Hash         string // pk: gateway.HashToken of the raw token
	GitHubUserID int64
	GitHubLogin  string
	Created      time.Time
	LastUsed     time.Time // zero = never used
}

// TokenStore is the portal's writer-side view of the gateway token table.
// The gateway's read path (gateway.TokenStore) is untouched.
type TokenStore interface {
	// ByUser lists the rows owned by one GitHub user (normally 0 or 1).
	ByUser(ctx context.Context, userID int64) ([]TokenRow, error)
	// Replace atomically deletes the given hashes and writes row — the
	// revoke-old + mint-new step of a regenerate. deleteHashes may be
	// empty (first mint).
	Replace(ctx context.Context, deleteHashes []string, row TokenRow) error
	// All lists every row, for the re-validation sweep.
	All(ctx context.Context) ([]TokenRow, error)
	// Delete revokes one token. Deleting a missing row is not an error.
	Delete(ctx context.Context, hash string) error
}

// NewToken mints a raw Shuck token from r (nil = crypto/rand). The raw
// value is shown to the user exactly once and never stored or logged; only
// gateway.HashToken(raw) persists.
func NewToken(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	buf := make([]byte, tokenBytes)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("read token randomness: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// Mint issues a fresh token for the user, atomically revoking any existing
// ones. regenerated reports whether prior tokens were revoked (for audit
// wording); raw is the token to show once.
func Mint(ctx context.Context, store TokenStore, r io.Reader, now time.Time, userID int64, login string) (raw string, regenerated bool, err error) {
	raw, err = NewToken(r)
	if err != nil {
		return "", false, err
	}
	existing, err := store.ByUser(ctx, userID)
	if err != nil {
		return "", false, fmt.Errorf("list existing tokens: %w", err)
	}
	old := make([]string, 0, len(existing))
	for _, row := range existing {
		old = append(old, row.Hash)
	}
	row := TokenRow{
		Hash:         gateway.HashToken(raw),
		GitHubUserID: userID,
		GitHubLogin:  login,
		Created:      now,
	}
	if err := store.Replace(ctx, old, row); err != nil {
		return "", false, fmt.Errorf("store token: %w", err)
	}
	return raw, len(old) > 0, nil
}
