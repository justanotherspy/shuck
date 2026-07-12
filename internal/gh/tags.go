package gh

import (
	"context"
	"fmt"

	"github.com/google/go-github/v89/github"

	"github.com/justanotherspy/shuck/internal/model"
)

// ListActionTags returns every tag in a repository paired with the commit SHA
// it resolves to, so a GitHub Actions `uses:` reference can be pinned to an
// immutable SHA. GitHub's tags listing reports the peeled commit SHA for
// annotated tags (what a workflow actually checks out), which is exactly what
// we want for pinning.
func (c *Client) ListActionTags(ctx context.Context, owner, repo string) ([]model.ActionTag, error) {
	opts := &github.ListOptions{PerPage: 100}
	var out []model.ActionTag
	for {
		tags, resp, err := c.gh.Repositories.ListTags(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list tags for %s/%s: %w", owner, repo, err)
		}
		for _, t := range tags {
			name, sha := t.GetName(), t.GetCommit().GetSHA()
			if name == "" || sha == "" {
				continue
			}
			out = append(out, model.ActionTag{Name: name, SHA: sha})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}
