package gh

import (
	"context"
	"fmt"
)

// AuthenticatedUser resolves the identity behind the client's token: the
// immutable numeric user ID and the current login. Used by the v2 token
// portal with a GitHub App user-access token; the CLI never calls it.
func (c *Client) AuthenticatedUser(ctx context.Context) (id int64, login string, err error) {
	u, _, err := c.gh.Users.Get(ctx, "")
	if err != nil {
		return 0, "", fmt.Errorf("get authenticated user: %w", err)
	}
	return u.GetID(), u.GetLogin(), nil
}

// OrgMember reports whether login is a member of org. go-github maps
// GitHub's boolean-by-status answer for us (204 member, 404 not — a 404 is a
// definitive "no", never an error); any other failure is an error the caller
// must treat as "unknown", not "false".
func (c *Client) OrgMember(ctx context.Context, org, login string) (bool, error) {
	member, _, err := c.gh.Organizations.IsMember(ctx, org, login)
	if err != nil {
		return false, fmt.Errorf("check %s org membership for %s: %w", org, login, err)
	}
	return member, nil
}

// UserLoginByID resolves the current login behind an immutable numeric user
// ID. Logins are mutable — a rename leaves any stored login stale — so
// callers that persist identities must re-resolve before probing by login.
// found is false only on a 404 (the account no longer exists — a definitive
// answer); any other failure is an error the caller must treat as "unknown",
// never as "gone".
func (c *Client) UserLoginByID(ctx context.Context, id int64) (login string, found bool, err error) {
	u, _, err := c.gh.Users.GetByID(ctx, id)
	if err != nil {
		if IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("resolve login for user %d: %w", id, err)
	}
	return u.GetLogin(), true, nil
}
