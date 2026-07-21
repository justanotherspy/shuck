package worker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/justanotherspy/shuck/internal/distil"
	"github.com/justanotherspy/shuck/internal/gateway"
	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/ingest"
)

// ErrGone marks a review-kind fetch whose object was deleted between the
// webhook and the fetch. Retrying cannot help, so the processor consumes the
// envelope without delivering (the content no longer exists).
var ErrGone = errors.New("worker: deleted between webhook and fetch")

// ReviewFetcher fetches a review-kind envelope's material. The production
// implementation is GHFetcher over internal/gh.
type ReviewFetcher interface {
	FetchReviewComment(ctx context.Context, token string, env ingest.Envelope) (ReviewCommentData, error)
	FetchReview(ctx context.Context, token string, env ingest.Envelope) (ReviewData, error)
}

// ReviewParent is the shape of the review a comment belongs to — the facts
// the standalone-wrapper rule needs. Found is false when the comment has no
// parent review id or the review is gone (both treated as standalone).
type ReviewParent struct {
	State    string
	Body     string
	Comments int
	Found    bool
}

// ReviewCommentData is what a ReviewFetcher recovers for one review_comment
// envelope.
type ReviewCommentData struct {
	Comment gh.PRReviewComment
	Parent  ReviewParent
	// Thread holds the whole thread (root first, creation order) when the
	// comment is a reply; nil otherwise. Best-effort — a thread fetch
	// failure degrades to the bare comment.
	Thread []gh.PRReviewComment
	// FileContent is the whole commented file at the PR head; "" degrades
	// the summary to the diff hunk alone.
	FileContent string
}

// ReviewData is what a ReviewFetcher recovers for one review envelope.
type ReviewData struct {
	Review   gh.PRReview
	Comments []gh.PRReviewComment
}

// IgnoreAuthors is the worker's bot-loop guard: review events authored by a
// listed identity are dropped globally, before any token is minted — an
// agent commenting AS the App must not trigger anyone. (Its complement, an
// agent commenting as a user, is the gateway's per-subscriber self-authored
// suppression, not this.) The zero value ignores nobody.
type IgnoreAuthors struct {
	ids    map[int64]bool
	logins map[string]bool
}

// ParseIgnoreAuthors parses a comma-separated list of numeric GitHub user
// IDs and logins (e.g. "12345,shuck-app[bot]"). IDs are authoritative;
// logins match case-insensitively. A positive-integer token is ambiguous —
// GitHub permits all-digit logins — so it matches BOTH ways: as a user ID
// and as a login.
func ParseIgnoreAuthors(s string) IgnoreAuthors {
	var ia IgnoreAuthors
	for tok := range strings.SplitSeq(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if id, err := strconv.ParseInt(tok, 10, 64); err == nil && id > 0 {
			if ia.ids == nil {
				ia.ids = make(map[int64]bool)
			}
			ia.ids[id] = true
			// No continue: the token also enters the login set below, so a
			// bot whose login IS these digits can still be ignored.
		}
		if ia.logins == nil {
			ia.logins = make(map[string]bool)
		}
		ia.logins[strings.ToLower(tok)] = true
	}
	return ia
}

// Match reports whether the author is on the ignore list.
func (ia IgnoreAuthors) Match(id int64, login string) bool {
	if id > 0 && ia.ids[id] {
		return true
	}
	return login != "" && ia.logins[strings.ToLower(login)]
}

// standaloneWrapper reports whether a review is the implicit wrapper GitHub
// creates around a lone comment or thread reply: state "commented", no
// top-level body, at most one comment. GitHub fires BOTH a
// pull_request_review.submitted and pull_request_review_comment.created
// webhook for every comment action, so both worker arms apply this same
// API-derived rule — order-independently — to deliver exactly one event per
// human action: a standalone wrapper is delivered by the comment arm and
// skipped by the review arm; anything else is one coherent review event and
// its member comments are skipped.
func standaloneWrapper(state, body string, comments int) bool {
	return strings.EqualFold(state, "commented") &&
		strings.TrimSpace(body) == "" && comments <= 1
}

// processReviewComment is the review_comment arm: bot-guard → fetch →
// standalone rule → distil → cap → deliver with the author identity.
func (p *Processor) processReviewComment(ctx context.Context, env ingest.Envelope) error {
	token, done, err := p.reviewSetup(ctx, env)
	if done || err != nil {
		return err
	}

	data, err := p.Reviews.FetchReviewComment(ctx, token, env)
	if errors.Is(err, ErrGone) {
		p.count(func(m *Metrics) { m.ReviewGone.Add(1) })
		p.log().Info("review comment gone before fetch", "delivery", env.DeliveryID,
			"repo", env.Repo, "pr", env.PR, "comment", env.CommentID)
		return nil
	}
	if err != nil {
		p.count(func(m *Metrics) { m.FetchErrors.Add(1) })
		return fmt.Errorf("fetch review comment %d for %s: %w", env.CommentID, env.Repo, err)
	}

	if data.Parent.Found && !standaloneWrapper(data.Parent.State, data.Parent.Body, data.Parent.Comments) {
		// Part of a bulk review: the review envelope delivers the whole
		// review as one coherent event; this fragment is a duplicate.
		p.count(func(m *Metrics) { m.DupSkipped.Add(1) })
		p.log().Info("review comment covered by its review", "delivery", env.DeliveryID,
			"repo", env.Repo, "pr", env.PR, "comment", env.CommentID, "review", data.Comment.ReviewID)
		return nil
	}

	res, err := distil.ReviewComment(p.reviewCommentInput(data))
	if err != nil {
		p.count(func(m *Metrics) { m.ParseErrors.Add(1) })
		return fmt.Errorf("distil review comment %d: %w", env.CommentID, err)
	}
	if err := p.deliverReview(ctx, env, gateway.KindReviewComment, res.Summary); err != nil {
		return err
	}
	p.count(func(m *Metrics) { m.ReviewComments.Add(1) })
	p.log().Info("delivered review_comment", "delivery", env.DeliveryID, "repo", env.Repo,
		"pr", env.PR, "comment", env.CommentID, "author", env.AuthorID)
	return nil
}

// processReview is the review arm: bot-guard → fetch → standalone rule →
// distil the whole review as one coherent event → cap → deliver.
func (p *Processor) processReview(ctx context.Context, env ingest.Envelope) error {
	token, done, err := p.reviewSetup(ctx, env)
	if done || err != nil {
		return err
	}

	data, err := p.Reviews.FetchReview(ctx, token, env)
	if errors.Is(err, ErrGone) {
		p.count(func(m *Metrics) { m.ReviewGone.Add(1) })
		p.log().Info("review gone before fetch", "delivery", env.DeliveryID,
			"repo", env.Repo, "pr", env.PR, "review", env.ReviewID)
		return nil
	}
	if err != nil {
		p.count(func(m *Metrics) { m.FetchErrors.Add(1) })
		return fmt.Errorf("fetch review %d for %s: %w", env.ReviewID, env.Repo, err)
	}

	if standaloneWrapper(data.Review.State, data.Review.Body, len(data.Comments)) {
		// The implicit wrapper around a lone comment or reply: the
		// review_comment envelope delivers it with thread and file context.
		p.count(func(m *Metrics) { m.DupSkipped.Add(1) })
		p.log().Info("standalone review wrapper skipped", "delivery", env.DeliveryID,
			"repo", env.Repo, "pr", env.PR, "review", env.ReviewID)
		return nil
	}

	in := distil.ReviewInput{
		Reviewer: loginOr(data.Review.UserLogin, env.AuthorLogin),
		State:    data.Review.State,
		Body:     data.Review.Body,
		Comments: make([]distil.ReviewCommentInput, 0, len(data.Comments)),
	}
	for _, c := range data.Comments {
		in.Comments = append(in.Comments, distil.ReviewCommentInput{
			Reviewer:  c.UserLogin,
			Path:      c.Path,
			Line:      c.Line,
			StartLine: c.StartLine,
			Side:      c.Side,
			Body:      c.Body,
			DiffHunk:  c.DiffHunk,
		})
	}
	res, err := distil.Review(in)
	if err != nil {
		p.count(func(m *Metrics) { m.ParseErrors.Add(1) })
		return fmt.Errorf("distil review %d: %w", env.ReviewID, err)
	}
	if err := p.deliverReview(ctx, env, gateway.KindReview, res.Summary); err != nil {
		return err
	}
	p.count(func(m *Metrics) { m.Reviews.Add(1) })
	p.log().Info("delivered review", "delivery", env.DeliveryID, "repo", env.Repo,
		"pr", env.PR, "review", env.ReviewID, "comments", len(data.Comments), "author", env.AuthorID)
	return nil
}

// reviewSetup runs the shared front half of both review arms: the bot
// guard (done=true consumes the envelope silently), the config and
// installation checks, and the token mint.
func (p *Processor) reviewSetup(ctx context.Context, env ingest.Envelope) (token string, done bool, err error) {
	if p.IgnoreAuthors.Match(env.AuthorID, env.AuthorLogin) {
		p.count(func(m *Metrics) { m.BotDropped.Add(1) })
		p.log().Info("ignored bot-authored review event", "delivery", env.DeliveryID,
			"repo", env.Repo, "pr", env.PR, "author", env.AuthorID, "login", env.AuthorLogin)
		return "", true, nil
	}
	if p.Reviews == nil {
		p.count(func(m *Metrics) { m.Invalid.Add(1) })
		return "", false, fmt.Errorf("%s envelope %s: no ReviewFetcher configured", env.Kind, env.DeliveryID)
	}
	if env.InstallationID <= 0 {
		// Unmintable: the envelope can never be processed. Failing it lets
		// the redrive policy park it in the DLQ rather than silently drop.
		p.count(func(m *Metrics) { m.Invalid.Add(1) })
		return "", false, fmt.Errorf("%s envelope %s has no installation_id", env.Kind, env.DeliveryID)
	}
	token, err = p.Tokens.Token(ctx, env.InstallationID)
	if err != nil {
		p.count(func(m *Metrics) { m.TokenErrors.Add(1) })
		return "", false, fmt.Errorf("mint installation token: %w", err)
	}
	return token, false, nil
}

// reviewCommentInput maps fetched comment material to the distil input,
// dropping the triggering comment itself from the quoted thread.
func (p *Processor) reviewCommentInput(data ReviewCommentData) distil.ReviewCommentInput {
	in := distil.ReviewCommentInput{
		Reviewer:     data.Comment.UserLogin,
		Path:         data.Comment.Path,
		Line:         data.Comment.Line,
		StartLine:    data.Comment.StartLine,
		Side:         data.Comment.Side,
		Body:         data.Comment.Body,
		DiffHunk:     data.Comment.DiffHunk,
		FileContent:  data.FileContent,
		ContextLines: p.ContextLines,
	}
	for _, tc := range data.Thread {
		if tc.ID == data.Comment.ID {
			continue
		}
		in.Thread = append(in.Thread, distil.ThreadComment{Author: tc.UserLogin, Body: tc.Body})
	}
	return in
}

// deliverReview caps and delivers a review-kind summary. The author comes
// from the envelope — the webhook payload is the authoritative identity —
// and is always present (envelope validation requires author_id), so the
// gateway's kind-scoped self-authored suppression always has its key.
func (p *Processor) deliverReview(ctx context.Context, env ingest.Envelope, kind gateway.EventKind, summary string) error {
	limit := p.SummaryLimit
	if limit == 0 {
		limit = distil.DefaultSummaryLimit
	}
	capped, truncated := distil.CapSummary(summary, limit, "[summary truncated]")
	if truncated {
		p.count(func(m *Metrics) { m.Truncated.Add(1) })
	}
	return p.deliver(ctx, gateway.DeliverRequest{
		Schema:  gateway.DeliverSchema,
		EventID: env.DeliveryID,
		Kind:    kind,
		Repo:    env.Repo,
		PR:      env.PR,
		Summary: capped,
		Author:  &gateway.Author{GitHubUserID: env.AuthorID, Login: env.AuthorLogin},
	})
}

// loginOr prefers the freshly fetched login, falling back to the webhook's.
func loginOr(fetched, envelope string) string {
	if fetched != "" {
		return fetched
	}
	return envelope
}
