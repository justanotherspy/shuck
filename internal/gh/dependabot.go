package gh

import (
	"context"
	"fmt"
)

// RepoTree lists a repository's file paths (repo-relative, slash-separated) at
// the given ref, using the Git Trees API recursively. An empty ref resolves to
// the default branch. Only blobs (files) are returned, not directory entries —
// enough for shuck to detect which package ecosystems a repo uses. Very large
// trees may be truncated by the API; the returned paths are still usable for
// ecosystem detection, which keys off shallow manifest files.
func (c *Client) RepoTree(ctx context.Context, owner, repo, ref string) ([]string, error) {
	if ref == "" {
		sha, err := c.DefaultBranchSHA(ctx, owner, repo)
		if err != nil {
			return nil, err
		}
		ref = sha
	}
	tree, _, err := c.gh.Git.GetTree(ctx, owner, repo, ref, true)
	if err != nil {
		return nil, fmt.Errorf("get file tree for %s/%s@%s: %w", owner, repo, ref, err)
	}
	var paths []string
	for _, e := range tree.Entries {
		if e.GetType() == "blob" {
			paths = append(paths, e.GetPath())
		}
	}
	return paths, nil
}
