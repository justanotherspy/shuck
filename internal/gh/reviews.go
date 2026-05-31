package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

const graphQLEndpoint = "https://api.github.com/graphql"

// aiReviewers lists substrings that mark a reviewer login as an AI code-review
// tool rather than a generic bot. It is a heuristic, deliberately easy to extend.
var aiReviewers = []string{
	"copilot", "claude", "cursor", "gemini", "codex", "devin",
	"coderabbit", "sourcery", "qodo", "codium", "ellipsis", "sweep",
}

// classifyAuthor labels a reviewer as AI, bot, or human from its login and the
// GraphQL actor __typename. AI wins over bot so a bot-typed AI tool is flagged
// as AI; the [bot] login suffix and a "Bot" typename both mark a plain bot.
func classifyAuthor(login, typename string) model.AuthorType {
	l := strings.ToLower(login)
	for _, name := range aiReviewers {
		if strings.Contains(l, name) {
			return model.AuthorAI
		}
	}
	if typename == "Bot" || strings.HasSuffix(l, "[bot]") {
		return model.AuthorBot
	}
	return model.AuthorHuman
}

// graphQL POSTs a query to GitHub's GraphQL API and unmarshals the data payload
// into out. A non-empty errors array is surfaced as a Go error.
func (c *Client) graphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphQLEndpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("graphql request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("graphql request: status %s", resp.Status)
	}

	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("graphql decode: %w", err)
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("graphql: %s", env.Errors[0].Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

type gqlPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type gqlActor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

// ReviewsFingerprint returns a cheap signature of a PR's review state, used to
// decide whether the full (expensive) review pull can be skipped. It folds the
// PR's updatedAt with review/thread counts and the number of resolved/outdated
// threads, so new reviews, new threads, replies (via updatedAt), and
// resolve/outdate flips all change it.
func (c *Client) ReviewsFingerprint(ctx context.Context, owner, repo string, number int) (string, error) {
	const query = `query($owner:String!,$repo:String!,$number:Int!){
	  repository(owner:$owner,name:$repo){
	    pullRequest(number:$number){
	      updatedAt
	      reviews{ totalCount }
	      reviewThreads(first:100){
	        totalCount
	        nodes{ isResolved isOutdated }
	      }
	    }
	  }
	}`
	var resp struct {
		Repository struct {
			PullRequest struct {
				UpdatedAt string `json:"updatedAt"`
				Reviews   struct {
					TotalCount int `json:"totalCount"`
				} `json:"reviews"`
				ReviewThreads struct {
					TotalCount int `json:"totalCount"`
					Nodes      []struct {
						IsResolved bool `json:"isResolved"`
						IsOutdated bool `json:"isOutdated"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": owner, "repo": repo, "number": number}
	if err := c.graphQL(ctx, query, vars, &resp); err != nil {
		return "", err
	}
	pr := resp.Repository.PullRequest
	var resolved, outdated int
	for _, t := range pr.ReviewThreads.Nodes {
		if t.IsResolved {
			resolved++
		}
		if t.IsOutdated {
			outdated++
		}
	}
	return fmt.Sprintf("%s|r%d|t%d|res%d|out%d",
		pr.UpdatedAt, pr.Reviews.TotalCount, pr.ReviewThreads.TotalCount, resolved, outdated), nil
}

// rawReview and rawThread are the decoded GraphQL shapes that feed buildReviews;
// keeping the assembly off the wire types makes it unit-testable without network.
type rawReview struct {
	ID          string
	State       string
	Body        string
	SubmittedAt string
	Author      gqlActor
}

type rawThreadComment struct {
	Body     string
	Author   gqlActor
	ReviewID string
}

type rawThread struct {
	Path       string
	Line       int
	IsResolved bool
	IsOutdated bool
	ResolvedBy string
	Comments   []rawThreadComment
}

// PRReviews fetches a PR's submitted reviews and inline threads via GraphQL and
// assembles them into the model, grouping threads under their originating review
// and collapsing resolved/outdated threads. commentLimit caps the comments shown
// per active thread.
func (c *Client) PRReviews(ctx context.Context, owner, repo string, number, commentLimit int) ([]model.Review, error) {
	reviews, err := c.fetchReviews(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	threads, err := c.fetchThreads(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	return buildReviews(reviews, threads, commentLimit), nil
}

func (c *Client) fetchReviews(ctx context.Context, owner, repo string, number int) ([]rawReview, error) {
	const query = `query($owner:String!,$repo:String!,$number:Int!,$after:String){
	  repository(owner:$owner,name:$repo){
	    pullRequest(number:$number){
	      reviews(first:100, after:$after){
	        pageInfo{ hasNextPage endCursor }
	        nodes{
	          id
	          state
	          body
	          submittedAt
	          author{ login __typename }
	        }
	      }
	    }
	  }
	}`
	var out []rawReview
	after := ""
	for {
		var resp struct {
			Repository struct {
				PullRequest struct {
					Reviews struct {
						PageInfo gqlPageInfo `json:"pageInfo"`
						Nodes    []struct {
							ID          string    `json:"id"`
							State       string    `json:"state"`
							Body        string    `json:"body"`
							SubmittedAt string    `json:"submittedAt"`
							Author      *gqlActor `json:"author"`
						} `json:"nodes"`
					} `json:"reviews"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "repo": repo, "number": number, "after": cursor(after)}
		if err := c.graphQL(ctx, query, vars, &resp); err != nil {
			return nil, err
		}
		rs := resp.Repository.PullRequest.Reviews
		for _, n := range rs.Nodes {
			out = append(out, rawReview{
				ID:          n.ID,
				State:       n.State,
				Body:        n.Body,
				SubmittedAt: n.SubmittedAt,
				Author:      actor(n.Author),
			})
		}
		if !rs.PageInfo.HasNextPage {
			break
		}
		after = rs.PageInfo.EndCursor
	}
	return out, nil
}

func (c *Client) fetchThreads(ctx context.Context, owner, repo string, number int) ([]rawThread, error) {
	const query = `query($owner:String!,$repo:String!,$number:Int!,$after:String){
	  repository(owner:$owner,name:$repo){
	    pullRequest(number:$number){
	      reviewThreads(first:100, after:$after){
	        pageInfo{ hasNextPage endCursor }
	        nodes{
	          isResolved
	          isOutdated
	          path
	          line
	          resolvedBy{ login }
	          comments(first:100){
	            nodes{
	              body
	              author{ login __typename }
	              pullRequestReview{ id }
	            }
	          }
	        }
	      }
	    }
	  }
	}`
	var out []rawThread
	after := ""
	for {
		var resp struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo gqlPageInfo `json:"pageInfo"`
						Nodes    []struct {
							IsResolved bool   `json:"isResolved"`
							IsOutdated bool   `json:"isOutdated"`
							Path       string `json:"path"`
							Line       int    `json:"line"`
							ResolvedBy *struct {
								Login string `json:"login"`
							} `json:"resolvedBy"`
							Comments struct {
								Nodes []struct {
									Body              string    `json:"body"`
									Author            *gqlActor `json:"author"`
									PullRequestReview *struct {
										ID string `json:"id"`
									} `json:"pullRequestReview"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "repo": repo, "number": number, "after": cursor(after)}
		if err := c.graphQL(ctx, query, vars, &resp); err != nil {
			return nil, err
		}
		ts := resp.Repository.PullRequest.ReviewThreads
		for _, n := range ts.Nodes {
			t := rawThread{
				Path:       n.Path,
				Line:       n.Line,
				IsResolved: n.IsResolved,
				IsOutdated: n.IsOutdated,
			}
			if n.ResolvedBy != nil {
				t.ResolvedBy = n.ResolvedBy.Login
			}
			for _, cm := range n.Comments.Nodes {
				rc := rawThreadComment{Body: cm.Body, Author: actor(cm.Author)}
				if cm.PullRequestReview != nil {
					rc.ReviewID = cm.PullRequestReview.ID
				}
				t.Comments = append(t.Comments, rc)
			}
			out = append(out, t)
		}
		if !ts.PageInfo.HasNextPage {
			break
		}
		after = ts.PageInfo.EndCursor
	}
	return out, nil
}

// cursor returns nil for the first page so the $after variable is sent as null.
func cursor(after string) any {
	if after == "" {
		return nil
	}
	return after
}

func actor(a *gqlActor) gqlActor {
	if a == nil {
		return gqlActor{}
	}
	return *a
}

// buildReviews groups threads under the review they originated in (the review of
// each thread's first comment) and collapses resolved/outdated threads. Pending
// reviews are skipped; a thread whose originating review is absent is dropped.
func buildReviews(reviews []rawReview, threads []rawThread, commentLimit int) []model.Review {
	out := make([]model.Review, 0, len(reviews))
	idx := map[string]int{} // review id -> position in out
	for _, r := range reviews {
		state := normalizeReviewState(r.State)
		if state == "" {
			continue // pending or unknown
		}
		idx[r.ID] = len(out)
		out = append(out, model.Review{
			Author:      r.Author.Login,
			AuthorType:  classifyAuthor(r.Author.Login, r.Author.Typename),
			State:       state,
			Body:        r.Body,
			SubmittedAt: parseTime(r.SubmittedAt),
		})
	}

	for _, t := range threads {
		if len(t.Comments) == 0 {
			continue
		}
		pos, ok := idx[t.Comments[0].ReviewID]
		if !ok {
			continue
		}
		out[pos].Threads = append(out[pos].Threads, summarizeThread(t, commentLimit))
	}

	// Drop empty "commented" reviews (e.g. the wrapper GitHub creates for a bare
	// reply): a comment verdict with no body and no originating threads carries
	// no information. An empty approved/changes_requested verdict is meaningful,
	// so those are kept.
	result := out[:0]
	for _, rv := range out {
		if rv.State == "commented" && rv.Body == "" && len(rv.Threads) == 0 {
			continue
		}
		result = append(result, rv)
	}
	return result
}

func summarizeThread(t rawThread, commentLimit int) model.ReviewThread {
	mt := model.ReviewThread{
		Path:          t.Path,
		Line:          t.Line,
		Resolved:      t.IsResolved,
		Outdated:      t.IsOutdated,
		TotalComments: len(t.Comments),
	}
	switch {
	case t.IsResolved:
		mt.Collapsed = true
		if t.ResolvedBy != "" {
			mt.CollapseReason = "resolved by " + t.ResolvedBy
		} else {
			mt.CollapseReason = "resolved"
		}
	case t.IsOutdated:
		mt.Collapsed = true
		mt.CollapseReason = "outdated"
	}
	if mt.Collapsed {
		return mt
	}

	// always keep the first comment of an active thread
	limit := max(commentLimit, 1)
	for i, cm := range t.Comments {
		if i >= limit {
			break
		}
		mt.Comments = append(mt.Comments, model.ReviewComment{
			Author:     cm.Author.Login,
			AuthorType: classifyAuthor(cm.Author.Login, cm.Author.Typename),
			Body:       cm.Body,
		})
	}
	if hidden := len(t.Comments) - limit; hidden > 0 {
		mt.HiddenComments = hidden
	}
	return mt
}

// parseTime parses an RFC3339 GraphQL timestamp, returning the zero time on any
// parse failure (a missing timestamp is not worth failing the whole report).
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func normalizeReviewState(state string) string {
	switch strings.ToUpper(state) {
	case "APPROVED":
		return "approved"
	case "CHANGES_REQUESTED":
		return "changes_requested"
	case "COMMENTED":
		return "commented"
	case "DISMISSED":
		return "dismissed"
	default:
		return "" // PENDING or unknown — not a submitted verdict
	}
}
