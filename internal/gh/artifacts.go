package gh

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/google/go-github/v89/github"

	"github.com/justanotherspy/shuck/internal/model"
)

// RunArtifacts lists the artifacts a workflow run uploaded
// (actions/upload-artifact). Artifacts are cheap run-level metadata; expired
// ones are still listed (their archives can no longer be downloaded).
func (c *Client) RunArtifacts(ctx context.Context, owner, repo string, runID int64) ([]model.Artifact, error) {
	opts := &github.ListOptions{PerPage: 100}
	var out []model.Artifact
	for {
		arts, resp, err := c.gh.Actions.ListWorkflowRunArtifacts(ctx, owner, repo, runID, opts)
		if err != nil {
			return nil, fmt.Errorf("list artifacts for run %d: %w", runID, err)
		}
		for _, a := range arts.Artifacts {
			out = append(out, model.Artifact{
				ID:        a.GetID(),
				RunID:     runID,
				Name:      a.GetName(),
				SizeBytes: a.GetSizeInBytes(),
				Expired:   a.GetExpired(),
				CreatedAt: a.GetCreatedAt().Time,
				ExpiresAt: a.GetExpiresAt().Time,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// ArtifactArchive downloads an artifact's zip archive by following the
// short-lived redirect GitHub returns (the same dance as JobLog). The caller
// owns the returned stream and must close it.
func (c *Client) ArtifactArchive(ctx context.Context, owner, repo string, artifactID int64) (io.ReadCloser, error) {
	u, _, err := c.gh.Actions.DownloadArtifact(ctx, owner, repo, artifactID, 4)
	if err != nil {
		return nil, fmt.Errorf("get archive URL for artifact %d: %w", artifactID, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download artifact %d: %w", artifactID, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download artifact %d: status %s", artifactID, resp.Status)
	}
	return resp.Body, nil
}
