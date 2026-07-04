// Package worker implements the event-worker core of shuck's opt-in
// self-hosted mode (JUS-87, JUS-91): consume a queue envelope
// (internal/ingest's contract), mint a GitHub App installation token, fetch
// the failed run's jobs and logs — or a review comment / submitted review
// with its context — distil them with the shared parser (internal/distil),
// and deliver the capped summary to the gateway's /internal/deliver endpoint
// (internal/gateway's contract). The package is pure Go with narrow
// interfaces for everything that touches the outside world — the AWS
// adapters live in worker/awsx and the binary in cmd/shuck-worker — so the
// portable shuck CLI never links any of it (see docs/V2.md for the
// compatibility contract).
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/justanotherspy/shuck/internal/distil"
	"github.com/justanotherspy/shuck/internal/gateway"
	"github.com/justanotherspy/shuck/internal/ingest"
	"github.com/justanotherspy/shuck/internal/model"
)

// TokenSource mints (or serves from cache) a GitHub App installation token
// for the installation an envelope belongs to.
type TokenSource interface {
	Token(ctx context.Context, installationID int64) (string, error)
}

// RunFetcher fetches a failed workflow run's drillable jobs with their raw
// logs. The production implementation is GHFetcher over internal/gh.
type RunFetcher interface {
	FetchRun(ctx context.Context, token string, env ingest.Envelope) (RunFailure, error)
}

// LogStore archives a job's whole raw log (the summary only carries the
// distilled excerpt). PutRawLog returns the archived object's URL. The
// production implementation is awsx.S3LogStore; retention is a lifecycle
// rule on the bucket (JUS-92), not worker logic.
type LogStore interface {
	PutRawLog(ctx context.Context, repo string, runID, jobID int64, log string) (string, error)
}

// Deliverer hands a distilled event to the gateway. The production
// implementation is HTTPDeliverer.
type Deliverer interface {
	Deliver(ctx context.Context, req gateway.DeliverRequest) error
}

// RunFailure is what a RunFetcher recovers for one ci_failure envelope.
type RunFailure struct {
	// Jobs are the run's failed and cancelled jobs, in API order.
	Jobs []JobFailure
	// RateRemaining is the shared REST quota remaining after the fetch;
	// -1 when unknown.
	RateRemaining int
}

// JobFailure is one drillable job's raw material for distillation.
type JobFailure struct {
	ID         int64
	Name       string
	Conclusion string
	Steps      []model.StepOverview
	// RawLog is the whole plain-text job log; empty when it could not be
	// downloaded (LogError then says why).
	RawLog string
	// LogError describes a failed log download. Log availability degrades
	// per job — one missing log must not sink the run's summary.
	LogError string
}

// Processor runs envelopes end to end. It is the single core both
// entrypoints share: the awsx SQS poll loop and the Lambda event handler
// call ProcessMessage per queue message.
type Processor struct {
	Tokens  TokenSource
	Fetch   RunFetcher
	Deliver Deliverer
	// Reviews fetches review-kind envelopes' material (JUS-91); nil fails
	// review envelopes with a config error.
	Reviews ReviewFetcher
	// Logs may be nil, which disables raw-log archiving.
	Logs LogStore
	// IgnoreAuthors is the bot-loop guard: review events authored by a
	// listed identity are dropped before any fetch. Zero value ignores
	// nobody.
	IgnoreAuthors IgnoreAuthors
	// ContextLines is how many file lines surround a review comment in its
	// summary; 0 means distil.DefaultContextLines.
	ContextLines int
	// SummaryLimit caps the delivered summary in bytes; 0 means
	// distil.DefaultSummaryLimit, negative means unlimited.
	SummaryLimit int
	// Options tunes distillation; the zero value means distil.DefaultOptions().
	Options *distil.Options
	// Log may be nil, which means slog.Default().
	Log *slog.Logger
	// Metrics may be nil, which disables counting.
	Metrics *Metrics
}

// ProcessMessage parses one queue message body and processes its envelope.
// A returned error means the message must be redelivered (the poll loop
// leaves it on the queue; the Lambda handler reports it as a batch item
// failure) — persistent failures reach the DLQ via the queue's redrive
// policy.
func (p *Processor) ProcessMessage(ctx context.Context, body []byte) error {
	env, err := ingest.ParseEnvelope(body)
	if err != nil {
		p.count(func(m *Metrics) { m.Invalid.Add(1) })
		return fmt.Errorf("parse envelope: %w", err)
	}
	return p.Process(ctx, env)
}

// Process runs one envelope end to end. Both delivered kinds use the
// envelope's delivery GUID as the deliver event_id, so worker retries are
// absorbed by the gateway's event_id dedupe instead of double-notifying.
func (p *Processor) Process(ctx context.Context, env ingest.Envelope) error {
	p.count(func(m *Metrics) { m.Received.Add(1) })
	switch env.Kind {
	case ingest.KindPRClosed:
		return p.processPRClosed(ctx, env)
	case ingest.KindCIFailure:
		return p.processCIFailure(ctx, env)
	case ingest.KindReviewComment:
		return p.processReviewComment(ctx, env)
	case ingest.KindReview:
		return p.processReview(ctx, env)
	default:
		// ParseEnvelope rejects unknown kinds; this only guards direct calls.
		p.count(func(m *Metrics) { m.Invalid.Add(1) })
		return fmt.Errorf("unknown envelope kind %q", env.Kind)
	}
}

// processPRClosed passes a pr_closed envelope straight to the gateway — no
// fetch, no token. Delivering it is what removes the PR's subscriptions
// (JUS-88).
func (p *Processor) processPRClosed(ctx context.Context, env ingest.Envelope) error {
	req := gateway.DeliverRequest{
		Schema:  gateway.DeliverSchema,
		EventID: env.DeliveryID,
		Kind:    gateway.KindPRClosed,
		Repo:    env.Repo,
		PR:      env.PR,
		Summary: "pull request closed",
	}
	if err := p.deliver(ctx, req); err != nil {
		return err
	}
	p.count(func(m *Metrics) { m.PRClosed.Add(1) })
	p.log().Info("delivered pr_closed", "delivery", env.DeliveryID, "repo", env.Repo, "pr", env.PR)
	return nil
}

// processCIFailure is the fetch → distil → cap → deliver pipeline.
func (p *Processor) processCIFailure(ctx context.Context, env ingest.Envelope) error {
	if env.InstallationID <= 0 {
		// Unmintable: the envelope can never be processed. Failing it lets
		// the redrive policy park it in the DLQ rather than silently drop.
		p.count(func(m *Metrics) { m.Invalid.Add(1) })
		return fmt.Errorf("ci_failure envelope %s has no installation_id", env.DeliveryID)
	}

	token, err := p.Tokens.Token(ctx, env.InstallationID)
	if err != nil {
		p.count(func(m *Metrics) { m.TokenErrors.Add(1) })
		return fmt.Errorf("mint installation token: %w", err)
	}

	fetchStart := time.Now()
	run, err := p.Fetch.FetchRun(ctx, token, env)
	if err != nil {
		p.count(func(m *Metrics) { m.FetchErrors.Add(1) })
		return fmt.Errorf("fetch run %d for %s: %w", env.RunID, env.Repo, err)
	}
	p.count(func(m *Metrics) {
		m.FetchLatencySumMS.Add(time.Since(fetchStart).Milliseconds())
		m.FetchLatencyCount.Add(1)
	})
	if run.RateRemaining >= 0 {
		p.count(func(m *Metrics) { m.RateRemaining.Store(int64(run.RateRemaining)) })
	}

	summary, archiveDir, err := p.distilRun(ctx, env, run)
	if err != nil {
		return err
	}

	limit := p.SummaryLimit
	if limit == 0 {
		limit = distil.DefaultSummaryLimit
	}
	capped, truncated := distil.CapSummary(summary, limit, truncationNote(archiveDir))
	if truncated {
		p.count(func(m *Metrics) { m.Truncated.Add(1) })
	}

	req := gateway.DeliverRequest{
		Schema:  gateway.DeliverSchema,
		EventID: env.DeliveryID,
		Kind:    gateway.KindCIFailure,
		Repo:    env.Repo,
		PR:      env.PR,
		Summary: capped,
	}
	if err := p.deliver(ctx, req); err != nil {
		return err
	}
	p.log().Info("delivered ci_failure", "delivery", env.DeliveryID, "repo", env.Repo,
		"pr", env.PR, "run", env.RunID, "jobs", len(run.Jobs), "truncated", truncated)
	return nil
}

// distilRun archives and parses each job, returning the joined summary and
// the archive directory URL of the last stored log ("" when none was).
func (p *Processor) distilRun(ctx context.Context, env ingest.Envelope, run RunFailure) (summary, archiveDir string, err error) {
	if len(run.Jobs) == 0 {
		// The run re-ran or its logs expired between webhook and fetch.
		// Deliver closure rather than dropping: the event_id is spent either
		// way, and the subscriber is waiting on this failure.
		return fmt.Sprintf("run %d: no failed jobs found at fetch time (re-run or logs expired)", env.RunID), "", nil
	}

	summaries := make([]string, 0, len(run.Jobs))
	for _, job := range run.Jobs {
		if p.Logs != nil && job.RawLog != "" {
			url, perr := p.Logs.PutRawLog(ctx, env.Repo, env.RunID, job.ID, job.RawLog)
			if perr != nil {
				p.count(func(m *Metrics) { m.LogArchiveErrors.Add(1) })
				p.log().Warn("raw-log archive failed", "delivery", env.DeliveryID,
					"repo", env.Repo, "job", job.ID, "err", perr)
			} else {
				p.count(func(m *Metrics) { m.LogsArchived.Add(1) })
				if i := strings.LastIndexByte(url, '/'); i > 0 {
					archiveDir = url[:i+1]
				}
			}
		}

		if job.RawLog == "" && job.LogError != "" {
			summaries = append(summaries, fmt.Sprintf("%s: %s — logs unavailable (%s)",
				jobName(job), job.Conclusion, job.LogError))
			continue
		}

		parseStart := time.Now()
		res, derr := distil.CIFailure(distil.Input{
			JobName:       job.Name,
			JobConclusion: job.Conclusion,
			Steps:         job.Steps,
			RawLog:        job.RawLog,
			Options:       p.options(),
		})
		if derr != nil {
			// Only invalid Options reach here — an operator config bug, not
			// a per-envelope condition.
			p.count(func(m *Metrics) { m.ParseErrors.Add(1) })
			return "", "", fmt.Errorf("distil job %d: %w", job.ID, derr)
		}
		p.count(func(m *Metrics) {
			m.ParseLatencySumMS.Add(time.Since(parseStart).Milliseconds())
			m.ParseLatencyCount.Add(1)
		})
		summaries = append(summaries, res.Summary)
	}
	return strings.Join(summaries, "\n\n"), archiveDir, nil
}

// truncationNote is the cap's marker line: it tells the reading agent the
// summary is partial and, when the raw logs were archived, where the whole
// thing lives.
func truncationNote(archiveDir string) string {
	if archiveDir == "" {
		return "[summary truncated]"
	}
	return "[summary truncated — full logs: " + archiveDir + "]"
}

func jobName(job JobFailure) string {
	if job.Name == "" {
		return "job"
	}
	return job.Name
}

func (p *Processor) deliver(ctx context.Context, req gateway.DeliverRequest) error {
	if err := p.Deliver.Deliver(ctx, req); err != nil {
		p.count(func(m *Metrics) { m.DeliverErrors.Add(1) })
		return fmt.Errorf("deliver event %s: %w", req.EventID, err)
	}
	p.count(func(m *Metrics) { m.Delivered.Add(1) })
	return nil
}

func (p *Processor) options() distil.Options {
	if p.Options == nil {
		return distil.DefaultOptions()
	}
	return *p.Options
}

func (p *Processor) log() *slog.Logger {
	if p.Log == nil {
		return slog.Default()
	}
	return p.Log
}

func (p *Processor) count(f func(*Metrics)) {
	if p.Metrics != nil {
		f(p.Metrics)
	}
}
