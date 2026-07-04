package worker

import (
	"context"
	"fmt"

	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/ingest"
)

// FetchReviewComment fetches one review comment plus its context: the
// parent review's shape (for the standalone-wrapper rule), the whole thread
// when replying, and the commented file at the PR head. Thread and file
// fetches degrade — the comment itself is the event; the parent-review
// fetch does not (both worker arms must derive the same standalone verdict,
// so an unreadable parent is retried, while a deleted one degrades to
// standalone).
func (f *GHFetcher) FetchReviewComment(ctx context.Context, token string, env ingest.Envelope) (ReviewCommentData, error) {
	owner, repo, c, err := f.client(token, env)
	if err != nil {
		return ReviewCommentData{}, err
	}

	comment, err := c.PRReviewComment(ctx, owner, repo, env.CommentID)
	if err != nil {
		if gh.IsNotFound(err) {
			return ReviewCommentData{}, fmt.Errorf("review comment %d: %w", env.CommentID, ErrGone)
		}
		return ReviewCommentData{}, err
	}
	data := ReviewCommentData{Comment: comment}

	if comment.ReviewID > 0 {
		parent, perr := c.PRReview(ctx, owner, repo, env.PR, comment.ReviewID)
		switch {
		case perr == nil:
			siblings, serr := c.PRReviewComments(ctx, owner, repo, env.PR, comment.ReviewID)
			if serr != nil {
				return ReviewCommentData{}, serr
			}
			data.Parent = ReviewParent{State: parent.State, Body: parent.Body, Comments: len(siblings), Found: true}
		case gh.IsNotFound(perr):
			// Review deleted: the review arm will see the same 404 and skip,
			// so treating the comment as standalone keeps single-delivery.
		default:
			return ReviewCommentData{}, perr
		}
	}

	if comment.InReplyTo > 0 {
		thread, terr := c.PRCommentThread(ctx, owner, repo, env.PR, comment.InReplyTo)
		if terr != nil {
			f.log().Warn("thread fetch failed", "repo", env.Repo, "pr", env.PR,
				"comment", env.CommentID, "err", terr)
		} else {
			data.Thread = thread
		}
	}

	data.FileContent = f.fileAtHead(ctx, c, owner, repo, env, comment)
	return data, nil
}

// FetchReview fetches one submitted review and all its inline comments.
func (f *GHFetcher) FetchReview(ctx context.Context, token string, env ingest.Envelope) (ReviewData, error) {
	owner, repo, c, err := f.client(token, env)
	if err != nil {
		return ReviewData{}, err
	}

	review, err := c.PRReview(ctx, owner, repo, env.PR, env.ReviewID)
	if err != nil {
		if gh.IsNotFound(err) {
			return ReviewData{}, fmt.Errorf("review %d: %w", env.ReviewID, ErrGone)
		}
		return ReviewData{}, err
	}
	comments, err := c.PRReviewComments(ctx, owner, repo, env.PR, env.ReviewID)
	if err != nil {
		return ReviewData{}, err
	}
	return ReviewData{Review: review, Comments: comments}, nil
}

// fileAtHead fetches the commented file for surrounding context, preferring
// the envelope's head SHA and falling back to the SHA the comment was
// anchored to. Best-effort: LEFT-side comments (head line numbers don't
// apply), path-less comments, and any fetch failure degrade to "" — the
// diff hunk still carries the essentials.
func (f *GHFetcher) fileAtHead(ctx context.Context, c *gh.Client, owner, repo string, env ingest.Envelope, comment gh.PRReviewComment) string {
	if comment.Path == "" || comment.Line <= 0 || comment.Side == "LEFT" {
		return ""
	}
	ref := env.HeadSHA
	if ref == "" {
		ref = comment.CommitID
	}
	content, err := c.FileContent(ctx, owner, repo, comment.Path, ref)
	if err != nil {
		f.log().Warn("file context fetch failed", "repo", env.Repo, "pr", env.PR,
			"path", comment.Path, "ref", ref, "err", err)
		return ""
	}
	return string(content)
}

// client resolves the envelope's repo and builds a token-authenticated
// GitHub client.
func (f *GHFetcher) client(token string, env ingest.Envelope) (owner, repo string, c *gh.Client, err error) {
	owner, repo, ok := splitRepo(env.Repo)
	if !ok {
		return "", "", nil, fmt.Errorf("envelope repo %q is not owner/name", env.Repo)
	}
	c, err = gh.NewEnterprise(token, f.APIBase)
	if err != nil {
		return "", "", nil, err
	}
	return owner, repo, c, nil
}
