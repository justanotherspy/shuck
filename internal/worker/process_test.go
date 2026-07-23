package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/distil"
	"github.com/justanotherspy/shuck/internal/gateway"
	"github.com/justanotherspy/shuck/internal/ingest"
	"github.com/justanotherspy/shuck/internal/logs"
	"github.com/justanotherspy/shuck/internal/model"
)

// fakeTokens is an in-memory TokenSource recording mint requests.
type fakeTokens struct {
	token string
	err   error
	calls []int64
}

func (f *fakeTokens) Token(_ context.Context, id int64) (string, error) {
	f.calls = append(f.calls, id)
	return f.token, f.err
}

// fakeFetch is an in-memory RunFetcher returning a scripted RunFailure.
type fakeFetch struct {
	run    RunFailure
	err    error
	tokens []string
	calls  int
}

func (f *fakeFetch) FetchRun(_ context.Context, token string, _ ingest.Envelope) (RunFailure, error) {
	f.calls++
	f.tokens = append(f.tokens, token)
	return f.run, f.err
}

// fakeLogs is an in-memory LogStore recording puts.
type fakeLogs struct {
	err  error
	puts []string // "repo/runID/jobID"
}

func (f *fakeLogs) PutRawLog(_ context.Context, repo string, runID, jobID int64, _ string) (string, error) {
	f.puts = append(f.puts, fmt.Sprintf("%s/%d/%d", repo, runID, jobID))
	if f.err != nil {
		return "", f.err
	}
	return fmt.Sprintf("s3://bucket/raw/%s/%d/%d.log", repo, runID, jobID), nil
}

// fakeDeliver is an in-memory Deliverer recording requests.
type fakeDeliver struct {
	err  error
	reqs []gateway.DeliverRequest
}

func (f *fakeDeliver) Deliver(_ context.Context, req gateway.DeliverRequest) error {
	f.reqs = append(f.reqs, req)
	return f.err
}

func failLog(step string) string {
	return "##[group]Run " + step + "\n##[endgroup]\n##[error]exit 1\n"
}

func twoJobRun() RunFailure {
	return RunFailure{
		RateRemaining: 4000,
		Jobs: []JobFailure{
			{ID: 1, Name: "test", Conclusion: "failure",
				Steps:  []model.StepOverview{{Number: 1, Name: "go test", Conclusion: "failure"}},
				RawLog: failLog("go test ./...")},
			{ID: 2, Name: "lint", Conclusion: "failure",
				Steps:  []model.StepOverview{{Number: 1, Name: "lint", Conclusion: "failure"}},
				RawLog: failLog("golangci-lint run")},
		},
	}
}

func TestProcessCIFailure(t *testing.T) {
	tokens := &fakeTokens{token: "ghs_x"}
	fetch := &fakeFetch{run: twoJobRun()}
	logStore := &fakeLogs{}
	deliver := &fakeDeliver{}
	metrics := &Metrics{}
	p := &Processor{Tokens: tokens, Fetch: fetch, Logs: logStore, Deliver: deliver, Metrics: metrics}

	if err := p.Process(context.Background(), ciEnvelope()); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if got := tokens.calls; len(got) != 1 || got[0] != 42 {
		t.Errorf("token minted for %v, want [42]", got)
	}
	if fetch.tokens[0] != "ghs_x" {
		t.Errorf("fetch used token %q", fetch.tokens[0])
	}
	if len(logStore.puts) != 2 {
		t.Errorf("archived %v, want both jobs", logStore.puts)
	}
	if len(deliver.reqs) != 1 {
		t.Fatalf("delivered %d times, want 1", len(deliver.reqs))
	}
	req := deliver.reqs[0]
	if req.EventID != "d-1" {
		t.Errorf("event_id = %q, want the delivery GUID (idempotency key)", req.EventID)
	}
	if req.Kind != gateway.KindCIFailure || req.Repo != "o/r" || req.PR != 7 || req.Schema != gateway.DeliverSchema {
		t.Errorf("request = %+v", req)
	}
	if err := req.Validate(); err != nil {
		t.Errorf("delivered request violates the contract: %v", err)
	}
	// Both jobs' summaries joined.
	if !strings.Contains(req.Summary, "test: failure") || !strings.Contains(req.Summary, "lint: failure") {
		t.Errorf("summary missing a job: %q", req.Summary)
	}
	if metrics.RateRemaining.Load() != 4000 {
		t.Errorf("rate gauge = %d", metrics.RateRemaining.Load())
	}
	if metrics.Delivered.Load() != 1 || metrics.Truncated.Load() != 0 {
		t.Errorf("delivered=%d truncated=%d", metrics.Delivered.Load(), metrics.Truncated.Load())
	}
}

func TestProcessCIFailureTruncates(t *testing.T) {
	run := twoJobRun()
	deliver := &fakeDeliver{}
	metrics := &Metrics{}
	p := &Processor{Tokens: &fakeTokens{token: "t"}, Fetch: &fakeFetch{run: run},
		Logs: &fakeLogs{}, Deliver: deliver, Metrics: metrics, SummaryLimit: 100}

	if err := p.Process(context.Background(), ciEnvelope()); err != nil {
		t.Fatalf("Process: %v", err)
	}
	sum := deliver.reqs[0].Summary
	if len(sum) > 100 {
		t.Errorf("summary %d bytes exceeds limit", len(sum))
	}
	// The note carries the archive location (jobs were archived).
	if !strings.Contains(sum, "full logs: s3://bucket/raw/o/r/99/") {
		t.Errorf("truncation note missing archive dir: %q", sum)
	}
	if metrics.Truncated.Load() != 1 {
		t.Errorf("truncation not counted")
	}
}

func TestProcessCIFailureArchiveFailureIsNonFatal(t *testing.T) {
	deliver := &fakeDeliver{}
	metrics := &Metrics{}
	p := &Processor{Tokens: &fakeTokens{token: "t"}, Fetch: &fakeFetch{run: twoJobRun()},
		Logs: &fakeLogs{err: errors.New("bucket gone")}, Deliver: deliver,
		Metrics: metrics, SummaryLimit: 100}

	if err := p.Process(context.Background(), ciEnvelope()); err != nil {
		t.Fatalf("archive failure must not fail the envelope: %v", err)
	}
	if len(deliver.reqs) != 1 {
		t.Fatal("event not delivered")
	}
	// Without a stored log, the truncation note has no s3 pointer.
	if sum := deliver.reqs[0].Summary; !strings.HasSuffix(sum, "[summary truncated]") {
		t.Errorf("summary note = %q", sum)
	}
	if metrics.LogArchiveErrors.Load() != 2 {
		t.Errorf("archive errors = %d, want 2", metrics.LogArchiveErrors.Load())
	}
}

func TestProcessCIFailureNilLogStore(t *testing.T) {
	deliver := &fakeDeliver{}
	p := &Processor{Tokens: &fakeTokens{token: "t"}, Fetch: &fakeFetch{run: twoJobRun()}, Deliver: deliver}
	if err := p.Process(context.Background(), ciEnvelope()); err != nil {
		t.Fatalf("nil LogStore must disable archiving, not fail: %v", err)
	}
}

func TestProcessCIFailureLogUnavailableJob(t *testing.T) {
	run := RunFailure{RateRemaining: -1, Jobs: []JobFailure{
		{ID: 2, Name: "lint", Conclusion: "cancelled", LogError: "status 404"},
	}}
	deliver := &fakeDeliver{}
	metrics := &Metrics{}
	p := &Processor{Tokens: &fakeTokens{token: "t"}, Fetch: &fakeFetch{run: run},
		Deliver: deliver, Metrics: metrics}

	if err := p.Process(context.Background(), ciEnvelope()); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if sum := deliver.reqs[0].Summary; !strings.Contains(sum, "logs unavailable (status 404)") {
		t.Errorf("summary = %q", sum)
	}
	// Unknown rate must not clobber the gauge.
	if metrics.RateRemaining.Load() != 0 {
		t.Errorf("rate gauge = %d, want untouched", metrics.RateRemaining.Load())
	}
}

func TestProcessCIFailureNoJobs(t *testing.T) {
	deliver := &fakeDeliver{}
	p := &Processor{Tokens: &fakeTokens{token: "t"}, Fetch: &fakeFetch{run: RunFailure{RateRemaining: -1}}, Deliver: deliver}
	if err := p.Process(context.Background(), ciEnvelope()); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if sum := deliver.reqs[0].Summary; !strings.Contains(sum, "no failed jobs found at fetch time") {
		t.Errorf("summary = %q", sum)
	}
}

func TestProcessCIFailureErrorsPropagate(t *testing.T) {
	base := func() *Processor {
		return &Processor{Tokens: &fakeTokens{token: "t"}, Fetch: &fakeFetch{run: twoJobRun()},
			Deliver: &fakeDeliver{}, Metrics: &Metrics{}}
	}

	t.Run("missing installation id", func(t *testing.T) {
		p := base()
		env := ciEnvelope()
		env.InstallationID = 0
		if err := p.Process(context.Background(), env); err == nil {
			t.Fatal("want error")
		}
		if p.Metrics.Invalid.Load() != 1 {
			t.Error("not counted invalid")
		}
	})
	t.Run("token error", func(t *testing.T) {
		p := base()
		p.Tokens = &fakeTokens{err: errors.New("401")}
		if err := p.Process(context.Background(), ciEnvelope()); err == nil {
			t.Fatal("want error")
		}
		if p.Metrics.TokenErrors.Load() != 1 {
			t.Error("not counted")
		}
	})
	t.Run("fetch error", func(t *testing.T) {
		p := base()
		p.Fetch = &fakeFetch{err: errors.New("api down")}
		if err := p.Process(context.Background(), ciEnvelope()); err == nil {
			t.Fatal("want error")
		}
		if p.Metrics.FetchErrors.Load() != 1 {
			t.Error("not counted")
		}
	})
	t.Run("deliver error", func(t *testing.T) {
		p := base()
		p.Deliver = &fakeDeliver{err: errors.New("gateway down")}
		if err := p.Process(context.Background(), ciEnvelope()); err == nil {
			t.Fatal("want error")
		}
		if p.Metrics.DeliverErrors.Load() != 1 {
			t.Error("not counted")
		}
	})
}

func TestProcessorValidate(t *testing.T) {
	tests := []struct {
		name    string
		p       *Processor
		wantErr bool
	}{
		{"zero value defaults", &Processor{}, false},
		{"valid custom config", &Processor{ContextLines: 5, Options: &distil.Options{Extract: logs.DefaultOptions()}}, false},
		{"negative context lines", &Processor{ContextLines: -1}, true},
		{"invalid options", &Processor{Options: &distil.Options{Extract: logs.Options{Context: -1}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.p.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestProcessCIFailureInvalidOptionsPoison(t *testing.T) {
	// Constructed directly, bypassing the boot-time Validate: an invalid
	// Options must error the envelope (redelivery → DLQ), count a parse
	// error, and deliver nothing.
	deliver := &fakeDeliver{}
	metrics := &Metrics{}
	p := &Processor{Tokens: &fakeTokens{token: "t"}, Fetch: &fakeFetch{run: twoJobRun()},
		Deliver: deliver, Metrics: metrics,
		Options: &distil.Options{Extract: logs.Options{Context: -1}}}

	if err := p.Process(context.Background(), ciEnvelope()); err == nil {
		t.Fatal("want error for invalid Options")
	}
	if metrics.ParseErrors.Load() != 1 {
		t.Errorf("parse errors = %d, want 1", metrics.ParseErrors.Load())
	}
	if len(deliver.reqs) != 0 || metrics.Delivered.Load() != 0 {
		t.Errorf("delivered %d times, want nothing on a distillation failure", len(deliver.reqs))
	}
}

func TestProcessPRClosedDeliverError(t *testing.T) {
	deliver := &fakeDeliver{err: errors.New("gateway down")}
	metrics := &Metrics{}
	p := &Processor{Tokens: &fakeTokens{token: "t"}, Fetch: &fakeFetch{},
		Deliver: deliver, Metrics: metrics}

	env := ingest.Envelope{Schema: ingest.EnvelopeSchema, DeliveryID: "d-2",
		Kind: ingest.KindPRClosed, Repo: "o/r", PR: 7}
	if err := p.Process(context.Background(), env); err == nil {
		t.Fatal("a deliver failure must propagate so the message redelivers")
	}
	if metrics.DeliverErrors.Load() != 1 {
		t.Errorf("deliver errors = %d, want 1", metrics.DeliverErrors.Load())
	}
	if metrics.PRClosed.Load() != 0 {
		t.Error("a failed pr_closed must not count as delivered")
	}
}

func TestProcessPRClosedPassesThrough(t *testing.T) {
	tokens := &fakeTokens{token: "t"}
	fetch := &fakeFetch{}
	deliver := &fakeDeliver{}
	p := &Processor{Tokens: tokens, Fetch: fetch, Deliver: deliver, Metrics: &Metrics{}}

	env := ingest.Envelope{Schema: ingest.EnvelopeSchema, DeliveryID: "d-2",
		Kind: ingest.KindPRClosed, Repo: "o/r", PR: 7}
	if err := p.Process(context.Background(), env); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(tokens.calls) != 0 || fetch.calls != 0 {
		t.Error("pr_closed must not mint or fetch")
	}
	req := deliver.reqs[0]
	if req.Kind != gateway.KindPRClosed || req.EventID != "d-2" || req.Validate() != nil {
		t.Errorf("request = %+v", req)
	}
	if p.Metrics.PRClosed.Load() != 1 {
		t.Error("pr_closed not counted")
	}
}

func TestProcessMessage(t *testing.T) {
	deliver := &fakeDeliver{}
	p := &Processor{Tokens: &fakeTokens{token: "t"}, Fetch: &fakeFetch{run: twoJobRun()},
		Deliver: deliver, Metrics: &Metrics{}}

	body, err := ciEnvelope().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ProcessMessage(context.Background(), body); err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	if len(deliver.reqs) != 1 {
		t.Fatal("not delivered")
	}

	if err := p.ProcessMessage(context.Background(), []byte("not json")); err == nil {
		t.Fatal("want parse error for a poison message")
	}
	if p.Metrics.Invalid.Load() != 1 {
		t.Error("poison message not counted invalid")
	}
}

func TestProcessUnknownKind(t *testing.T) {
	p := &Processor{Deliver: &fakeDeliver{}, Metrics: &Metrics{}}
	env := ciEnvelope()
	env.Kind = "mystery"
	if err := p.Process(context.Background(), env); err == nil {
		t.Fatal("want error for unknown kind")
	}
}
