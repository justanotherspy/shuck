package gh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-github/v89/github"
)

// FileContent fetches a single file's decoded contents from a repository at the
// given ref (empty ref = the repository's default branch).
//
// The monitor is the caller that matters: a review comment points at a path and
// a line, and an event is far more useful carrying the surrounding lines of the
// file as it stood at the PR head than the diff hunk alone. Fetching at an
// explicit ref is the whole point — the comment's own commit is what its line
// numbers refer to, and reading the default branch instead would quote the
// wrong code with total confidence.
func (c *Client) FileContent(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	var opts *github.RepositoryContentGetOptions
	if ref != "" {
		opts = &github.RepositoryContentGetOptions{Ref: ref}
	}
	fc, _, _, err := c.gh.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		return nil, fmt.Errorf("get %s from %s/%s: %w", path, owner, repo, err)
	}
	// A directory path decodes to a nil file and a non-nil slice; treat it as a
	// miss rather than returning empty content that reads like an empty file.
	if fc == nil {
		return nil, fmt.Errorf("%s in %s/%s is not a file", path, owner, repo)
	}
	content, err := fc.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decode %s from %s/%s: %w", path, owner, repo, err)
	}
	return []byte(content), nil
}

// IsNotFound reports whether err is a GitHub 404 — a missing repository, file,
// or endpoint. `shuck security` uses it to tell a repository that does not
// exist from one whose alert sources are merely all disabled, which would
// otherwise render as a false all-clear.
func IsNotFound(err error) bool {
	var ghErr *github.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
}

// FileNotFound reports whether err is a 404 from a content fetch, so a caller
// after an optional file can tell "there is no such file" from "the fetch
// broke". The monitor leans on the distinction: a comment on a file deleted in
// a later commit is normal and merely costs the event its code context, while a
// real API failure must not be silently rendered as a file that is not there.
//
// It adds a substring fallback to [IsNotFound] because the contents endpoint is
// the one that has been seen to surface a 404 the type assertion does not
// reach; erring toward "not found" is the safe direction here, since the caller
// degrades to less context rather than to a wrong answer.
func FileNotFound(err error) bool {
	return IsNotFound(err) || strings.Contains(err.Error(), "404")
}
