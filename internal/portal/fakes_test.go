package portal

import (
	"context"
	"fmt"
	"sync"
)

// fakeStore is an in-memory TokenStore recording operations.
type fakeStore struct {
	mu       sync.Mutex
	rows     map[string]TokenRow // hash -> row
	ops      []string
	listErr  error
	writeErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: make(map[string]TokenRow)}
}

func (f *fakeStore) ByUser(_ context.Context, userID int64) ([]TokenRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []TokenRow
	for _, row := range f.rows {
		if row.GitHubUserID == userID {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeStore) Replace(_ context.Context, deleteHashes []string, row TokenRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, fmt.Sprintf("replace:%d:%s", len(deleteHashes), row.Hash))
	if f.writeErr != nil {
		return f.writeErr
	}
	for _, hash := range deleteHashes {
		delete(f.rows, hash)
	}
	f.rows[row.Hash] = row
	return nil
}

func (f *fakeStore) All(_ context.Context) ([]TokenRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []TokenRow
	for _, row := range f.rows {
		out = append(out, row)
	}
	return out, nil
}

func (f *fakeStore) Delete(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "delete:"+hash)
	if f.writeErr != nil {
		return f.writeErr
	}
	delete(f.rows, hash)
	return nil
}

func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func (f *fakeStore) has(hash string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.rows[hash]
	return ok
}

// fakeValidator scripts membership answers.
type fakeValidator struct {
	member bool
	err    error
	calls  int
}

func (f *fakeValidator) Member(context.Context, int64, string) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	return f.member, nil
}

// fakeGitHub scripts the GitHub authorizer.
type fakeGitHub struct {
	exchangeErr error
	userErr     error
	userID      int64
	login       string
	gotCode     string
}

func (f *fakeGitHub) AuthURL(state, redirectURI string) string {
	return "https://github.example/authorize?state=" + state + "&redirect_uri=" + redirectURI
}

func (f *fakeGitHub) Exchange(_ context.Context, code, _ string) (string, error) {
	f.gotCode = code
	if f.exchangeErr != nil {
		return "", f.exchangeErr
	}
	return "gh-user-token", nil
}

func (f *fakeGitHub) User(_ context.Context, _ string) (int64, string, error) {
	if f.userErr != nil {
		return 0, "", f.userErr
	}
	return f.userID, f.login, nil
}

// fakeOIDC scripts the SSO gate.
type fakeOIDC struct {
	verifyErr error
	gotNonce  string
}

func (f *fakeOIDC) AuthURL(state, nonce, redirectURI string) string {
	return "https://idp.example/authorize?state=" + state + "&nonce=" + nonce + "&redirect_uri=" + redirectURI
}

func (f *fakeOIDC) Verify(_ context.Context, _, nonce, _ string) (string, error) {
	f.gotNonce = nonce
	if f.verifyErr != nil {
		return "", f.verifyErr
	}
	return "subject-1", nil
}
