package portal

import (
	"context"
	"errors"
	"fmt"
)

// Validator decides whether a verified GitHub user may hold a Shuck token.
// GitHub org membership (or account ownership, for personal installs) is
// the access-control plane.
type Validator interface {
	// Member reports whether the user is currently allowed. An error means
	// "unknown" — callers must treat it as a skipped check, never as false
	// (the sweep must not revoke on a flaky API call).
	Member(ctx context.Context, userID int64, login string) (bool, error)
}

// InstallationTokenSource mints GitHub App installation tokens. Satisfied
// by *worker.AppTokenSource.
type InstallationTokenSource interface {
	Token(ctx context.Context, installationID int64) (string, error)
}

// OrgAPI is the pair of GitHub calls the org check needs. Satisfied by
// *gh.Client.
type OrgAPI interface {
	OrgMember(ctx context.Context, org, login string) (bool, error)
	// UserLoginByID resolves the current login behind an immutable numeric
	// user ID. found is false only when the account no longer exists (a
	// definitive answer); any other failure is an error meaning "unknown".
	UserLoginByID(ctx context.Context, id int64) (login string, found bool, err error)
}

// OrgValidator checks membership of the installation's org with a
// members:read installation token.
type OrgValidator struct {
	Org            string
	InstallationID int64
	Tokens         InstallationTokenSource
	// NewClient builds the API client for one check from a fresh (cached)
	// installation token. Injected so tests never hit the network.
	NewClient func(token string) (OrgAPI, error)
}

// Member implements Validator via the org members probe. The probe is by
// login, but logins are mutable: a stored login goes stale when the user
// renames, and probing it would 404 into a definitive "not a member" — a
// false revoke for the sweep. So when a numeric user ID is known it wins:
// the current login is re-resolved from the immutable ID first, and only a
// deleted account (a definitive lookup 404) is a non-member; any lookup
// error stays "unknown".
func (v *OrgValidator) Member(ctx context.Context, userID int64, login string) (bool, error) {
	if userID <= 0 && login == "" {
		return false, errors.New("no user id or login to validate")
	}
	token, err := v.Tokens.Token(ctx, v.InstallationID)
	if err != nil {
		return false, fmt.Errorf("installation token: %w", err)
	}
	client, err := v.NewClient(token)
	if err != nil {
		return false, fmt.Errorf("build client: %w", err)
	}
	if userID > 0 {
		fresh, found, err := client.UserLoginByID(ctx, userID)
		if err != nil {
			return false, fmt.Errorf("resolve current login for user %d: %w", userID, err)
		}
		if !found {
			// The account is gone — definitively not a member.
			return false, nil
		}
		login = fresh
	}
	return client.OrgMember(ctx, v.Org, login)
}

// AccountValidator is the personal-install mode: only the account owner may
// hold a token. Local and infallible — Member never errors.
type AccountValidator struct {
	// AccountID is the installation account's immutable numeric user ID.
	AccountID int64
}

// Member implements Validator by comparing numeric user IDs.
func (v *AccountValidator) Member(_ context.Context, userID int64, _ string) (bool, error) {
	return userID == v.AccountID, nil
}
