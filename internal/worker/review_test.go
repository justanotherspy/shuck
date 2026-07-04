package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/gateway"
	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/ingest"
)

// fakeReviews is an in-memory ReviewFetcher returning scripted data.
type fakeReviews struct {
	comment      ReviewCommentData
	commentErr   error
	review       ReviewData
	reviewErr    error
	tokens       []string
	commentCalls int
	reviewCalls  int
}

func (f *fakeReviews) FetchReviewComment(_ context.Context, token string, _ ingest.Envelope) (ReviewCommentData, error) {
	f.commentCalls++
	f.tokens = append(f.tokens, token)
	return f.comment, f.commentErr
}

func (f *fakeReviews) FetchReview(_ context.Context, token string, _ ingest.Envelope) (ReviewData, error) {
	f.reviewCalls++
	f.tokens = append(f.tokens, token)
	return f.review, f.reviewErr
}

func rcEnvelope() ingest.Envelope {
	return ingest.Envelope{
		Schema:         ingest.EnvelopeSchema,
		DeliveryID:     "d-3",
		Kind:           ingest.KindReviewComment,
		Repo:           "o/r",
		PR:             7,
		HeadSHA:        "head1",
		InstallationID: 42,
		CommentID:      9001,
		AuthorID:       555,
		AuthorLogin:    "octocat",
	}
}

func rvEnvelope() ingest.Envelope {
	return ingest.Envelope{
		Schema:         ingest.EnvelopeSchema,
		DeliveryID:     "d-4",
		Kind:           ingest.KindReview,
		Repo:           "o/r",
		PR:             7,
		HeadSHA:        "head1",
		InstallationID: 42,
		ReviewID:       77,
		AuthorID:       555,
		AuthorLogin:    "octocat",
	}
}

// standaloneCommentData is the fetch result for a lone comment: its parent
// review is the implicit "commented, no body, one comment" wrapper.
func standaloneCommentData() ReviewCommentData {
	return ReviewCommentData{
		Comment: gh.PRReviewComment{
			ID: 9001, ReviewID: 77, Path: "a.go", Line: 3, Side: "RIGHT",
			Body: "rename this", DiffHunk: "@@ -1 +1 @@\n-x\n+y",
			UserID: 555, UserLogin: "octocat",
		},
		Parent:      ReviewParent{State: "COMMENTED", Body: "", Comments: 1, Found: true},
		FileContent: "l1\nl2\nl3\nl4\nl5\n",
	}
}

func bulkReviewData() ReviewData {
	return ReviewData{
		Review: gh.PRReview{ID: 77, State: "CHANGES_REQUESTED", Body: "Needs work.", UserID: 555, UserLogin: "octocat"},
		Comments: []gh.PRReviewComment{
			{ID: 1, ReviewID: 77, Path: "a.go", Line: 3, Body: "first", UserLogin: "octocat"},
			{ID: 2, ReviewID: 77, Path: "b.go", Line: 9, Body: "second", UserLogin: "octocat"},
			{ID: 3, ReviewID: 77, Path: "c.go", Body: "third", UserLogin: "octocat"},
		},
	}
}

func reviewProcessor(reviews *fakeReviews) (*Processor, *fakeTokens, *fakeDeliver) {
	tokens := &fakeTokens{token: "ghs_x"}
	deliver := &fakeDeliver{}
	p := &Processor{Tokens: tokens, Fetch: &fakeFetch{}, Reviews: reviews,
		Deliver: deliver, Metrics: &Metrics{}}
	return p, tokens, deliver
}

func TestProcessReviewCommentStandaloneDelivers(t *testing.T) {
	data := standaloneCommentData()
	data.Comment.InReplyTo = 8000
	data.Thread = []gh.PRReviewComment{
		{ID: 8000, Body: "root question", UserLogin: "alice"},
		{ID: 9001, Body: "rename this", UserLogin: "octocat"}, // the comment itself
	}
	reviews := &fakeReviews{comment: data}
	p, tokens, deliver := reviewProcessor(reviews)

	if err := p.Process(context.Background(), rcEnvelope()); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := tokens.calls; len(got) != 1 || got[0] != 42 {
		t.Errorf("token minted for %v, want [42]", got)
	}
	if reviews.tokens[0] != "ghs_x" {
		t.Errorf("fetch used token %q", reviews.tokens[0])
	}
	if len(deliver.reqs) != 1 {
		t.Fatalf("delivered %d times, want 1", len(deliver.reqs))
	}
	req := deliver.reqs[0]
	if req.EventID != "d-3" {
		t.Errorf("event_id = %q, want the delivery GUID (idempotency key)", req.EventID)
	}
	if req.Kind != gateway.KindReviewComment || req.Repo != "o/r" || req.PR != 7 || req.Schema != gateway.DeliverSchema {
		t.Errorf("request = %+v", req)
	}
	if err := req.Validate(); err != nil {
		t.Errorf("delivered request violates the contract: %v", err)
	}
	if req.Author == nil || req.Author.GitHubUserID != 555 || req.Author.Login != "octocat" {
		t.Errorf("author = %+v, want the envelope identity", req.Author)
	}
	if !req.Suppressed("555") || req.Suppressed("556") {
		t.Error("gateway suppression must key on the delivered author")
	}
	for _, want := range []string{
		"Reviewer octocat commented on a.go:3:",
		"rename this",
		"> alice: root question", // thread quoted, own comment excluded
		"Diff hunk:",
		"3 | l3", // file context at head
	} {
		if !strings.Contains(req.Summary, want) {
			t.Errorf("summary missing %q:\n%s", want, req.Summary)
		}
	}
	if strings.Count(req.Summary, "rename this") != 1 {
		t.Error("the triggering comment must not be quoted into its own thread")
	}
	if p.Metrics.ReviewComments.Load() != 1 || p.Metrics.Delivered.Load() != 1 {
		t.Error("review_comment not counted")
	}
}

func TestProcessReviewCommentPartOfBulkReviewSkips(t *testing.T) {
	data := standaloneCommentData()
	data.Parent = ReviewParent{State: "CHANGES_REQUESTED", Body: "Needs work.", Comments: 3, Found: true}
	reviews := &fakeReviews{comment: data}
	p, _, deliver := reviewProcessor(reviews)

	if err := p.Process(context.Background(), rcEnvelope()); err != nil {
		t.Fatalf("a skipped duplicate must consume the envelope: %v", err)
	}
	if len(deliver.reqs) != 0 {
		t.Fatalf("delivered %d times, want 0 (covered by the review event)", len(deliver.reqs))
	}
	if p.Metrics.DupSkipped.Load() != 1 {
		t.Error("skip not counted")
	}
}

func TestProcessReviewCommentParentVariants(t *testing.T) {
	tests := []struct {
		name        string
		parent      ReviewParent
		wantDeliver bool
	}{
		{"standalone wrapper", ReviewParent{State: "COMMENTED", Comments: 1, Found: true}, true},
		{"wrapper with zero comments", ReviewParent{State: "COMMENTED", Comments: 0, Found: true}, true},
		{"whitespace body is still a wrapper", ReviewParent{State: "commented", Body: " \n", Comments: 1, Found: true}, true},
		{"parent gone", ReviewParent{}, true},
		{"review with body", ReviewParent{State: "COMMENTED", Body: "real review", Comments: 1, Found: true}, false},
		{"multi-comment review", ReviewParent{State: "COMMENTED", Comments: 2, Found: true}, false},
		{"verdict review", ReviewParent{State: "APPROVED", Comments: 1, Found: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := standaloneCommentData()
			data.Parent = tt.parent
			p, _, deliver := reviewProcessor(&fakeReviews{comment: data})
			if err := p.Process(context.Background(), rcEnvelope()); err != nil {
				t.Fatalf("Process: %v", err)
			}
			if got := len(deliver.reqs) == 1; got != tt.wantDeliver {
				t.Errorf("delivered = %v, want %v", got, tt.wantDeliver)
			}
		})
	}
}

func TestProcessReviewDeliversOneCoherentEvent(t *testing.T) {
	reviews := &fakeReviews{review: bulkReviewData()}
	p, tokens, deliver := reviewProcessor(reviews)

	if err := p.Process(context.Background(), rvEnvelope()); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := tokens.calls; len(got) != 1 || got[0] != 42 {
		t.Errorf("token minted for %v, want [42]", got)
	}
	if len(deliver.reqs) != 1 {
		t.Fatalf("delivered %d times, want exactly one coherent event", len(deliver.reqs))
	}
	req := deliver.reqs[0]
	if req.EventID != "d-4" || req.Kind != gateway.KindReview {
		t.Errorf("request = %+v", req)
	}
	if err := req.Validate(); err != nil {
		t.Errorf("delivered request violates the contract: %v", err)
	}
	if req.Author == nil || req.Author.GitHubUserID != 555 {
		t.Errorf("author = %+v", req.Author)
	}
	for _, want := range []string{
		"Reviewer octocat requested changes — 3 inline comment(s)",
		"Needs work.",
		"a.go:3:", "first",
		"b.go:9:", "second",
		"c.go:", "third",
	} {
		if !strings.Contains(req.Summary, want) {
			t.Errorf("summary missing %q:\n%s", want, req.Summary)
		}
	}
	if p.Metrics.Reviews.Load() != 1 {
		t.Error("review not counted")
	}
}

func TestProcessReviewStandaloneWrapperSkips(t *testing.T) {
	reviews := &fakeReviews{review: ReviewData{
		Review:   gh.PRReview{ID: 77, State: "COMMENTED", Body: "", UserID: 555},
		Comments: []gh.PRReviewComment{{ID: 9001, Body: "rename this"}},
	}}
	p, _, deliver := reviewProcessor(reviews)

	if err := p.Process(context.Background(), rvEnvelope()); err != nil {
		t.Fatalf("a skipped wrapper must consume the envelope: %v", err)
	}
	if len(deliver.reqs) != 0 {
		t.Fatalf("delivered %d times, want 0 (the comment envelope covers it)", len(deliver.reqs))
	}
	if p.Metrics.DupSkipped.Load() != 1 {
		t.Error("skip not counted")
	}
}

func TestProcessReviewApprovalWithoutCommentsDelivers(t *testing.T) {
	reviews := &fakeReviews{review: ReviewData{
		Review: gh.PRReview{ID: 77, State: "APPROVED", Body: "LGTM", UserID: 555, UserLogin: "octocat"},
	}}
	p, _, deliver := reviewProcessor(reviews)

	if err := p.Process(context.Background(), rvEnvelope()); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(deliver.reqs) != 1 {
		t.Fatal("a verdict review must deliver")
	}
	if !strings.Contains(deliver.reqs[0].Summary, "approved the pull request") {
		t.Errorf("summary = %q", deliver.reqs[0].Summary)
	}
}

func TestProcessReviewBotGuard(t *testing.T) {
	for _, tt := range []struct {
		name   string
		ignore string
	}{
		{"by numeric id", "555"},
		{"by login", "OctoCat"},
		{"in a list", "12, shuck[bot] ,555"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, env := range []ingest.Envelope{rcEnvelope(), rvEnvelope()} {
				reviews := &fakeReviews{comment: standaloneCommentData(), review: bulkReviewData()}
				p, tokens, deliver := reviewProcessor(reviews)
				p.IgnoreAuthors = ParseIgnoreAuthors(tt.ignore)

				if err := p.Process(context.Background(), env); err != nil {
					t.Fatalf("a bot drop must consume the envelope: %v", err)
				}
				if len(tokens.calls) != 0 || reviews.commentCalls+reviews.reviewCalls != 0 {
					t.Error("bot guard must run before any mint or fetch")
				}
				if len(deliver.reqs) != 0 {
					t.Error("bot-authored event must not deliver")
				}
				if p.Metrics.BotDropped.Load() != 1 {
					t.Error("drop not counted")
				}
			}
		})
	}
}

func TestParseIgnoreAuthors(t *testing.T) {
	var zero IgnoreAuthors
	if zero.Match(555, "octocat") {
		t.Error("zero value must ignore nobody")
	}
	empty := ParseIgnoreAuthors("")
	if empty.Match(555, "octocat") {
		t.Error("empty list must ignore nobody")
	}
	ia := ParseIgnoreAuthors(" 123 ,shuck-app[bot], 0, -5 ")
	tests := []struct {
		id    int64
		login string
		want  bool
	}{
		{123, "whoever", true},        // by id
		{999, "SHUCK-APP[bot]", true}, // by login, case-insensitive
		{999, "other", false},
		{0, "", false},  // "0" token is not a valid id
		{-5, "", false}, // negative ids never match
	}
	for _, tt := range tests {
		if got := ia.Match(tt.id, tt.login); got != tt.want {
			t.Errorf("Match(%d, %q) = %v, want %v", tt.id, tt.login, got, tt.want)
		}
	}
	// "0" and "-5" fall through to the login set rather than the id set.
	if !ia.Match(999, "0") || !ia.Match(999, "-5") {
		t.Error("non-positive numeric tokens are treated as logins")
	}
}

func TestProcessReviewGoneConsumes(t *testing.T) {
	t.Run("comment", func(t *testing.T) {
		p, _, deliver := reviewProcessor(&fakeReviews{
			commentErr: fmt.Errorf("review comment 9001: %w", ErrGone)})
		if err := p.Process(context.Background(), rcEnvelope()); err != nil {
			t.Fatalf("gone must consume, not redeliver: %v", err)
		}
		if len(deliver.reqs) != 0 || p.Metrics.ReviewGone.Load() != 1 {
			t.Error("gone comment must not deliver and must be counted")
		}
	})
	t.Run("review", func(t *testing.T) {
		p, _, deliver := reviewProcessor(&fakeReviews{
			reviewErr: fmt.Errorf("review 77: %w", ErrGone)})
		if err := p.Process(context.Background(), rvEnvelope()); err != nil {
			t.Fatalf("gone must consume, not redeliver: %v", err)
		}
		if len(deliver.reqs) != 0 || p.Metrics.ReviewGone.Load() != 1 {
			t.Error("gone review must not deliver and must be counted")
		}
	})
}

func TestProcessReviewErrorsPropagate(t *testing.T) {
	t.Run("no fetcher configured", func(t *testing.T) {
		p, _, _ := reviewProcessor(nil)
		p.Reviews = nil
		if err := p.Process(context.Background(), rcEnvelope()); err == nil {
			t.Fatal("want error")
		}
		if p.Metrics.Invalid.Load() != 1 {
			t.Error("not counted invalid")
		}
	})
	t.Run("missing installation id", func(t *testing.T) {
		p, _, _ := reviewProcessor(&fakeReviews{})
		env := rvEnvelope()
		env.InstallationID = 0
		if err := p.Process(context.Background(), env); err == nil {
			t.Fatal("want error")
		}
		if p.Metrics.Invalid.Load() != 1 {
			t.Error("not counted invalid")
		}
	})
	t.Run("token error", func(t *testing.T) {
		p, _, _ := reviewProcessor(&fakeReviews{comment: standaloneCommentData()})
		p.Tokens = &fakeTokens{err: errors.New("401")}
		if err := p.Process(context.Background(), rcEnvelope()); err == nil {
			t.Fatal("want error")
		}
		if p.Metrics.TokenErrors.Load() != 1 {
			t.Error("not counted")
		}
	})
	t.Run("fetch error", func(t *testing.T) {
		p, _, _ := reviewProcessor(&fakeReviews{commentErr: errors.New("api down")})
		if err := p.Process(context.Background(), rcEnvelope()); err == nil {
			t.Fatal("want error")
		}
		if p.Metrics.FetchErrors.Load() != 1 {
			t.Error("not counted")
		}
	})
	t.Run("deliver error", func(t *testing.T) {
		p, _, deliver := reviewProcessor(&fakeReviews{review: bulkReviewData()})
		deliver.err = errors.New("gateway down")
		if err := p.Process(context.Background(), rvEnvelope()); err == nil {
			t.Fatal("want error")
		}
		if p.Metrics.DeliverErrors.Load() != 1 {
			t.Error("not counted")
		}
	})
}

func TestProcessReviewCommentTruncates(t *testing.T) {
	data := standaloneCommentData()
	data.Comment.Body = strings.Repeat("long comment line\n", 50)
	p, _, deliver := reviewProcessor(&fakeReviews{comment: data})
	p.SummaryLimit = 120

	if err := p.Process(context.Background(), rcEnvelope()); err != nil {
		t.Fatalf("Process: %v", err)
	}
	sum := deliver.reqs[0].Summary
	if len(sum) > 120 {
		t.Errorf("summary %d bytes exceeds limit", len(sum))
	}
	if !strings.HasSuffix(sum, "[summary truncated]") {
		t.Errorf("summary note = %q", sum)
	}
	if p.Metrics.Truncated.Load() != 1 {
		t.Error("truncation not counted")
	}
}

func TestStandaloneWrapper(t *testing.T) {
	tests := []struct {
		state    string
		body     string
		comments int
		want     bool
	}{
		{"COMMENTED", "", 1, true},
		{"commented", " \n", 1, true},
		{"COMMENTED", "", 0, true},
		{"COMMENTED", "body", 1, false},
		{"COMMENTED", "", 2, false},
		{"APPROVED", "", 1, false},
		{"CHANGES_REQUESTED", "", 0, false},
	}
	for _, tt := range tests {
		if got := standaloneWrapper(tt.state, tt.body, tt.comments); got != tt.want {
			t.Errorf("standaloneWrapper(%q, %q, %d) = %v, want %v", tt.state, tt.body, tt.comments, got, tt.want)
		}
	}
}
