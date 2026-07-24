package gh

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-github/v89/github"
)

// PRReviewComment is one pull-request review comment with the anchor and
// author material the background monitor feeds to distil. REST-sourced (the
// GraphQL reviews query serves the CLI's whole-PR view and carries neither
// diff hunks nor numeric ids).
type PRReviewComment struct {
	ID        int64
	ReviewID  int64
	InReplyTo int64
	Path      string
	// Line and StartLine anchor the comment on Side; both 0 for file-level
	// comments.
	Line      int
	StartLine int
	Side      string
	Body      string
	DiffHunk  string
	// CommitID is the head SHA the comment was anchored to.
	CommitID  string
	UserID    int64
	UserLogin string
	// CreatedAt and UpdatedAt let a poller ask for only what is new since it
	// last looked.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PRReview is one pull-request review's verdict material.
type PRReview struct {
	ID          int64
	State       string
	Body        string
	UserID      int64
	UserLogin   string
	SubmittedAt time.Time
}

// PRReviewComment fetches a single review comment by its comment ID.
func (c *Client) PRReviewComment(ctx context.Context, owner, repo string, commentID int64) (PRReviewComment, error) {
	rc, _, err := c.gh.PullRequests.GetComment(ctx, owner, repo, commentID)
	if err != nil {
		return PRReviewComment{}, fmt.Errorf("get review comment %d in %s/%s: %w", commentID, owner, repo, err)
	}
	return reviewComment(rc), nil
}

// PRReview fetches a single review of a pull request by its review ID.
func (c *Client) PRReview(ctx context.Context, owner, repo string, number int, reviewID int64) (PRReview, error) {
	rv, _, err := c.gh.PullRequests.GetReview(ctx, owner, repo, number, reviewID)
	if err != nil {
		return PRReview{}, fmt.Errorf("get review %d on %s/%s#%d: %w", reviewID, owner, repo, number, err)
	}
	return review(rv), nil
}

// PRReviewsSince lists a pull request's submitted reviews, keeping only those
// submitted strictly after since (a zero since keeps all of them). The reviews
// endpoint has no server-side filter, but a PR accumulates reviews slowly
// enough that listing and filtering is the cheaper design than tracking
// pagination state across polls.
func (c *Client) PRReviewsSince(ctx context.Context, owner, repo string, number int, since time.Time) ([]PRReview, error) {
	opts := &github.ListOptions{PerPage: 100}
	var out []PRReview
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("list reviews on %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, rv := range reviews {
			r := review(rv)
			// A review that has not been submitted is the caller's own
			// pending draft; it is not an event.
			if r.SubmittedAt.IsZero() || !r.SubmittedAt.After(since) {
				continue
			}
			out = append(out, r)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// PRReviewCommentsSince lists a pull request's review comments created or
// updated after since, newest last. Unlike the reviews endpoint this one
// filters server-side, so a poll that finds nothing new costs one small
// response.
func (c *Client) PRReviewCommentsSince(ctx context.Context, owner, repo string, number int, since time.Time) ([]PRReviewComment, error) {
	opts := &github.PullRequestListCommentsOptions{
		Sort:        "created",
		Direction:   "asc",
		Since:       since,
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var out []PRReviewComment
	for {
		comments, resp, err := c.gh.PullRequests.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("list review comments on %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, rc := range comments {
			out = append(out, reviewComment(rc))
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

func review(rv *github.PullRequestReview) PRReview {
	return PRReview{
		ID:          rv.GetID(),
		State:       rv.GetState(),
		Body:        rv.GetBody(),
		UserID:      rv.GetUser().GetID(),
		UserLogin:   rv.GetUser().GetLogin(),
		SubmittedAt: rv.GetSubmittedAt().Time,
	}
}

// PRReviewComments lists every comment belonging to one review, in API
// order (paginated).
func (c *Client) PRReviewComments(ctx context.Context, owner, repo string, number int, reviewID int64) ([]PRReviewComment, error) {
	opts := &github.ListOptions{PerPage: 100}
	var out []PRReviewComment
	for {
		comments, resp, err := c.gh.PullRequests.ListReviewComments(ctx, owner, repo, number, reviewID, opts)
		if err != nil {
			return nil, fmt.Errorf("list comments of review %d on %s/%s#%d: %w", reviewID, owner, repo, number, err)
		}
		for _, rc := range comments {
			out = append(out, reviewComment(rc))
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// PRCommentThread reconstructs the review thread rooted at rootID: the root
// comment plus every reply, in creation order. GitHub flattens threads — a
// reply's in_reply_to_id is always the root comment's ID — so one pass over
// the PR's review comments (which the API sorts by creation) is exact.
func (c *Client) PRCommentThread(ctx context.Context, owner, repo string, number int, rootID int64) ([]PRReviewComment, error) {
	opts := &github.PullRequestListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	var out []PRReviewComment
	for {
		comments, resp, err := c.gh.PullRequests.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("list review comments on %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, rc := range comments {
			if rc.GetID() == rootID || rc.GetInReplyTo() == rootID {
				out = append(out, reviewComment(rc))
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

func reviewComment(rc *github.PullRequestComment) PRReviewComment {
	return PRReviewComment{
		ID:        rc.GetID(),
		ReviewID:  rc.GetPullRequestReviewID(),
		InReplyTo: rc.GetInReplyTo(),
		Path:      rc.GetPath(),
		Line:      rc.GetLine(),
		StartLine: rc.GetStartLine(),
		Side:      rc.GetSide(),
		Body:      rc.GetBody(),
		DiffHunk:  rc.GetDiffHunk(),
		CommitID:  rc.GetCommitID(),
		UserID:    rc.GetUser().GetID(),
		UserLogin: rc.GetUser().GetLogin(),
		CreatedAt: rc.GetCreatedAt().Time,
		UpdatedAt: rc.GetUpdatedAt().Time,
	}
}
