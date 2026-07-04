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

// OrgAPI is the one GitHub call the org check needs. Satisfied by
// *gh.Client.
type OrgAPI interface {
	OrgMember(ctx context.Context, org, login string) (bool, error)
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

// Member implements Validator via the org members probe.
func (v *OrgValidator) Member(ctx context.Context, _ int64, login string) (bool, error) {
	if login == "" {
		return false, errors.New("empty login")
	}
	token, err := v.Tokens.Token(ctx, v.InstallationID)
	if err != nil {
		return false, fmt.Errorf("installation token: %w", err)
	}
	client, err := v.NewClient(token)
	if err != nil {
		return false, fmt.Errorf("build client: %w", err)
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
