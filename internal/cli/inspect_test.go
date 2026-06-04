package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/justanotherspy/shuck/internal/cache"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/target"
)

// stubInspect is a configurable inspectClient that records per-method call
// counts so the cache-reuse and focus-mode behavior can be asserted without
// touching GitHub.
type stubInspect struct {
	pr        model.PR
	prErr     error
	prCalls   int
	openPR    int
	openErr   error
	openCalls int

	failed    []model.JobResult
	cancelled []model.JobResult
	running   []model.RunningJob
	jobsErr   error
	jobsCalls int

	other      []model.OtherCheck
	otherErr   error
	otherCalls int

	jobLog      string
	jobLogErr   error
	jobLogCalls int

	annotations []model.Annotation
	annErr      error
	annCalls    int

	fingerprint string
	fpErr       error
	fpCalls     int

	reviews      []model.Review
	reviewsErr   error
	reviewsCalls int

	runInfo   model.RunInfo
	runFailed []model.JobResult
	runCancel []model.JobResult
	runRun    []model.RunningJob
	runErr    error
	runCalls  int
}

func (s *stubInspect) GetPR(_ context.Context, _, _ string, _ int) (model.PR, error) {
	s.prCalls++
	return s.pr, s.prErr
}

func (s *stubInspect) FindOpenPR(_ context.Context, _, _, _, _ string) (int, error) {
	s.openCalls++
	return s.openPR, s.openErr
}

func (s *stubInspect) ListJobs(_ context.Context, _, _, _ string) (failed, cancelled []model.JobResult, running []model.RunningJob, err error) {
	s.jobsCalls++
	return cloneJobs(s.failed), cloneJobs(s.cancelled), s.running, s.jobsErr
}

func (s *stubInspect) OtherChecks(_ context.Context, _, _, _ string) ([]model.OtherCheck, error) {
	s.otherCalls++
	return s.other, s.otherErr
}

func (s *stubInspect) JobLog(_ context.Context, _, _ string, _ int64) (string, error) {
	s.jobLogCalls++
	return s.jobLog, s.jobLogErr
}

func (s *stubInspect) JobAnnotations(_ context.Context, _, _ string, _ int64) ([]model.Annotation, error) {
	s.annCalls++
	return s.annotations, s.annErr
}

func (s *stubInspect) ReviewsFingerprint(_ context.Context, _, _ string, _ int) (string, error) {
	s.fpCalls++
	return s.fingerprint, s.fpErr
}

func (s *stubInspect) PRReviews(_ context.Context, _, _ string, _, _ int) ([]model.Review, error) {
	s.reviewsCalls++
	return s.reviews, s.reviewsErr
}

func (s *stubInspect) RunReport(_ context.Context, _, _ string, _, _ int64) (info model.RunInfo, failed, cancelled []model.JobResult, running []model.RunningJob, err error) {
	s.runCalls++
	return s.runInfo, cloneJobs(s.runFailed), cloneJobs(s.runCancel), s.runRun, s.runErr
}

// cloneJobs deep-copies a job slice so a stubbed call returns fresh values that
// drill can mutate in place without polluting the stub's templates between runs.
func cloneJobs(in []model.JobResult) []model.JobResult {
	if in == nil {
		return nil
	}
	out := make([]model.JobResult, len(in))
	for i := range in {
		out[i] = in[i]
		if in[i].Steps != nil {
			out[i].Steps = append([]model.StepOverview(nil), in[i].Steps...)
		}
	}
	return out
}

// withStubInspect points newInspectClient at s and gives the test an isolated
// cache home plus a token (resolveToken errors without one).
func withStubInspect(t *testing.T, s *stubInspect) {
	t.Helper()
	t.Setenv("SHUCK_HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GH_TOKEN", "")
	prev := newInspectClient
	newInspectClient = func(string) inspectClient { return s }
	t.Cleanup(func() { newInspectClient = prev })
}

func failedJob() model.JobResult {
	return model.JobResult{
		ID: 1, RunAttempt: 1, Name: "build", Conclusion: "failure",
		Steps: []model.StepOverview{
			{Number: 1, Name: "Checkout", Conclusion: "success"},
			{Number: 2, Name: "Run tests", Conclusion: "failure"},
		},
	}
}

func ciStub() *stubInspect {
	return &stubInspect{
		pr:     model.PR{Owner: "o", Repo: "r", Number: 42, Title: "fix", HeadSHA: "abc1234"},
		failed: []model.JobResult{failedJob()},
		jobLog: failLog,
	}
}

func TestPRReportHappyPath(t *testing.T) {
	s := ciStub()
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", Number: 42}

	report, err := inspectWith(context.Background(), tgt, options{
		reviewCommentLimit: 5, ciOnly: true, context: 10, shortThreshold: 100, tail: 100,
	})
	if err != nil {
		t.Fatalf("inspectWith: %v", err)
	}
	if len(report.FailedJobs) != 1 {
		t.Fatalf("failed jobs = %d, want 1", len(report.FailedJobs))
	}
	fj := report.FailedJobs[0]
	if !fj.Inspected || len(fj.FailedSteps) != 1 || fj.FailedSteps[0].Name != "Run tests" {
		t.Errorf("drill produced unexpected steps: %+v", fj.FailedSteps)
	}
	if s.jobLogCalls != 1 {
		t.Errorf("JobLog calls = %d, want 1", s.jobLogCalls)
	}
	// The cache should now be populated.
	cached, err := cache.Load("o", "r", 42)
	if err != nil || cached == nil {
		t.Fatalf("cache not written: %v / %v", cached, err)
	}
}

func TestPRReportAttachesAnnotations(t *testing.T) {
	s := ciStub()
	// Give the failed job a check-run ID so annotations are fetched, and have
	// the stub return one.
	s.failed[0].CheckRunID = 99
	s.annotations = []model.Annotation{
		{Path: "main_test.go", StartLine: 12, Level: "failure", Message: "TestThing failed"},
	}
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", Number: 42}
	o := options{reviewCommentLimit: 5, ciOnly: true, context: 10, shortThreshold: 100, tail: 100}

	report, err := inspectWith(context.Background(), tgt, o)
	if err != nil {
		t.Fatalf("inspectWith: %v", err)
	}
	if s.annCalls != 1 {
		t.Fatalf("JobAnnotations calls = %d, want 1", s.annCalls)
	}
	if got := report.FailedJobs[0].Annotations; len(got) != 1 || got[0].Path != "main_test.go" {
		t.Fatalf("annotations not attached: %+v", got)
	}

	// Annotations are cheap metadata: even when the raw log is reused from cache
	// (no re-download), they are re-fetched.
	report2, err := inspectWith(context.Background(), tgt, o)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if s.jobLogCalls != 1 {
		t.Errorf("second run should reuse cached log: JobLog calls = %d, want 1", s.jobLogCalls)
	}
	if s.annCalls != 2 {
		t.Errorf("annotations should be re-fetched on cache reuse: calls = %d, want 2", s.annCalls)
	}
	if len(report2.FailedJobs[0].Annotations) != 1 {
		t.Errorf("annotations missing on cache-reuse run: %+v", report2.FailedJobs[0].Annotations)
	}
}

func TestPRReportFindOpenPR(t *testing.T) {
	s := ciStub()
	s.openPR = 42
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", Branch: "feature"} // Number == 0

	report, err := inspectWith(context.Background(), tgt, options{
		reviewCommentLimit: 5, ciOnly: true, context: 10, shortThreshold: 100, tail: 100,
	})
	if err != nil {
		t.Fatalf("inspectWith: %v", err)
	}
	if s.openCalls != 1 {
		t.Errorf("FindOpenPR calls = %d, want 1", s.openCalls)
	}
	if report.PR.Number != 42 {
		t.Errorf("PR number = %d, want 42", report.PR.Number)
	}
}

func TestPRReportFindOpenPRError(t *testing.T) {
	s := ciStub()
	s.openErr = errors.New("no open PR")
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", Branch: "feature"}

	if _, err := inspectWith(context.Background(), tgt, options{
		reviewCommentLimit: 5, ciOnly: true, context: 10, shortThreshold: 100, tail: 100,
	}); err == nil {
		t.Fatal("expected FindOpenPR error to propagate")
	}
}

func TestPRReportCacheReuse(t *testing.T) {
	s := ciStub()
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", Number: 42}
	o := options{reviewCommentLimit: 5, ciOnly: true, context: 10, shortThreshold: 100, tail: 100}

	if _, err := inspectWith(context.Background(), tgt, o); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if s.jobLogCalls != 1 {
		t.Fatalf("first run JobLog calls = %d, want 1", s.jobLogCalls)
	}
	// Second run on the same head SHA must reuse the cached raw job log.
	if _, err := inspectWith(context.Background(), tgt, o); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if s.jobLogCalls != 1 {
		t.Errorf("second run should reuse cached log: JobLog calls = %d, want 1", s.jobLogCalls)
	}
}

func TestPRReportRefreshAndNoCacheRedownload(t *testing.T) {
	s := ciStub()
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", Number: 42}
	o := options{reviewCommentLimit: 5, ciOnly: true, context: 10, shortThreshold: 100, tail: 100}

	if _, err := inspectWith(context.Background(), tgt, o); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// --refresh ignores the cache and re-downloads.
	refresh := o
	refresh.refresh = true
	if _, err := inspectWith(context.Background(), tgt, refresh); err != nil {
		t.Fatalf("refresh run: %v", err)
	}
	if s.jobLogCalls != 2 {
		t.Errorf("--refresh should re-download: JobLog calls = %d, want 2", s.jobLogCalls)
	}
	// --no-cache also re-downloads.
	nc := o
	nc.noCache = true
	if _, err := inspectWith(context.Background(), tgt, nc); err != nil {
		t.Fatalf("no-cache run: %v", err)
	}
	if s.jobLogCalls != 3 {
		t.Errorf("--no-cache should re-download: JobLog calls = %d, want 3", s.jobLogCalls)
	}
}

func TestPRReportErrorPaths(t *testing.T) {
	tgt := target.Target{Owner: "o", Repo: "r", Number: 42}
	o := options{reviewCommentLimit: 5, context: 10, shortThreshold: 100, tail: 100}

	t.Run("GetPR error", func(t *testing.T) {
		s := ciStub()
		s.prErr = errors.New("boom")
		withStubInspect(t, s)
		if _, err := inspectWith(context.Background(), tgt, o); err == nil {
			t.Fatal("expected GetPR error")
		}
	})
	t.Run("ListJobs error", func(t *testing.T) {
		s := ciStub()
		s.jobsErr = errors.New("boom")
		withStubInspect(t, s)
		if _, err := inspectWith(context.Background(), tgt, o); err == nil {
			t.Fatal("expected ListJobs error")
		}
	})
	t.Run("OtherChecks error", func(t *testing.T) {
		s := ciStub()
		s.otherErr = errors.New("boom")
		withStubInspect(t, s)
		if _, err := inspectWith(context.Background(), tgt, o); err == nil {
			t.Fatal("expected OtherChecks error")
		}
	})
}

func TestPRReportCancelledJobDegrades(t *testing.T) {
	s := &stubInspect{
		pr: model.PR{Owner: "o", Repo: "r", Number: 42, HeadSHA: "abc1234"},
		cancelled: []model.JobResult{{
			ID: 9, RunAttempt: 1, Name: "e2e", Conclusion: "cancelled",
			Steps: []model.StepOverview{{Number: 1, Name: "Run e2e", Conclusion: "cancelled"}},
		}},
		jobLogErr: errors.New("no log"),
	}
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", Number: 42}

	report, err := inspectWith(context.Background(), tgt, options{
		reviewCommentLimit: 5, ciOnly: true, context: 10, shortThreshold: 100, tail: 100,
	})
	if err != nil {
		t.Fatalf("inspectWith: %v", err)
	}
	if len(report.CancelledJobs) != 1 {
		t.Fatalf("cancelled jobs = %d, want 1", len(report.CancelledJobs))
	}
	// A cancelled job with no downloadable log degrades to a bare listing.
	if report.CancelledJobs[0].Inspected {
		t.Errorf("cancelled job with no log should not be Inspected")
	}
}

func TestPRReportFailedJobLogUnavailable(t *testing.T) {
	s := ciStub()
	s.jobLogErr = errors.New("403")
	s.jobLog = ""
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", Number: 42}

	report, err := inspectWith(context.Background(), tgt, options{
		reviewCommentLimit: 5, ciOnly: true, context: 10, shortThreshold: 100, tail: 100,
	})
	if err != nil {
		t.Fatalf("inspectWith: %v", err)
	}
	steps := report.FailedJobs[0].FailedSteps
	if len(steps) != 1 || steps[0].Name != "(logs unavailable)" {
		t.Errorf("failed-log job should degrade to (logs unavailable), got %+v", steps)
	}
}

func TestAttachReviewsFingerprintReuse(t *testing.T) {
	s := ciStub()
	s.fingerprint = "fp-1"
	s.reviews = []model.Review{{Author: "alice", State: "approved"}}
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", Number: 42}
	o := options{reviewCommentLimit: 5, context: 10, shortThreshold: 100, tail: 100}

	// First run fetches reviews and caches them.
	if _, err := inspectWith(context.Background(), tgt, o); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if s.reviewsCalls != 1 {
		t.Fatalf("first run PRReviews calls = %d, want 1", s.reviewsCalls)
	}
	// Second run: fingerprint unchanged → cached reviews reused, no PRReviews call.
	if _, err := inspectWith(context.Background(), tgt, o); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if s.reviewsCalls != 1 {
		t.Errorf("unchanged fingerprint should reuse cached reviews: PRReviews calls = %d, want 1", s.reviewsCalls)
	}

	// Fingerprint moves → reviews are re-fetched.
	s.fingerprint = "fp-2"
	if _, err := inspectWith(context.Background(), tgt, o); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if s.reviewsCalls != 2 {
		t.Errorf("moved fingerprint should re-fetch: PRReviews calls = %d, want 2", s.reviewsCalls)
	}
}

func TestAttachReviewsErrorsAreNonFatal(t *testing.T) {
	tgt := target.Target{Owner: "o", Repo: "r", Number: 42}
	o := options{reviewCommentLimit: 5, context: 10, shortThreshold: 100, tail: 100}

	t.Run("fingerprint error", func(t *testing.T) {
		s := ciStub()
		s.fpErr = errors.New("graphql down")
		withStubInspect(t, s)
		report, err := inspectWith(context.Background(), tgt, o)
		if err != nil {
			t.Fatalf("fingerprint error should not fail inspection: %v", err)
		}
		if len(report.Reviews) != 0 {
			t.Errorf("reviews should be empty on fingerprint error")
		}
		if s.reviewsCalls != 0 {
			t.Errorf("PRReviews should not run after a fingerprint error")
		}
	})
	t.Run("reviews error", func(t *testing.T) {
		s := ciStub()
		s.fingerprint = "fp"
		s.reviewsErr = errors.New("graphql down")
		withStubInspect(t, s)
		report, err := inspectWith(context.Background(), tgt, o)
		if err != nil {
			t.Fatalf("reviews error should not fail inspection: %v", err)
		}
		if len(report.Reviews) != 0 {
			t.Errorf("reviews should be empty on PRReviews error")
		}
	})
}

// TestPRReportFocusModesPreserveOtherDimension proves that a reviews-only run
// (after a CI run cached the jobs) keeps the cached CI half, and vice versa.
func TestPRReportFocusModesPreserveOtherDimension(t *testing.T) {
	s := ciStub()
	s.fingerprint = "fp"
	s.reviews = []model.Review{{Author: "bob", State: "changes_requested"}}
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", Number: 42}
	base := options{reviewCommentLimit: 5, context: 10, shortThreshold: 100, tail: 100}

	// CI-only run caches the failed jobs (no reviews).
	ciOnly := base
	ciOnly.ciOnly = true
	if _, err := inspectWith(context.Background(), tgt, ciOnly); err != nil {
		t.Fatalf("ci-only run: %v", err)
	}

	// reviews-only run renders only reviews but must persist the cached CI half.
	revOnly := base
	revOnly.reviewsOnly = true
	rep, err := inspectWith(context.Background(), tgt, revOnly)
	if err != nil {
		t.Fatalf("reviews-only run: %v", err)
	}
	if len(rep.FailedJobs) != 0 {
		t.Errorf("reviews-only render should not include CI jobs")
	}
	if s.jobsCalls != 1 {
		t.Errorf("reviews-only run should not list jobs: ListJobs calls = %d, want 1", s.jobsCalls)
	}

	// The cache must still carry both dimensions after the reviews-only run.
	cached, err := cache.Load("o", "r", 42)
	if err != nil || cached == nil {
		t.Fatalf("cache load: %v", err)
	}
	if len(cached.FailedJobs) != 1 {
		t.Errorf("reviews-only run clobbered cached CI half: %+v", cached.FailedJobs)
	}
	if len(cached.Reviews) != 1 {
		t.Errorf("reviews-only run did not persist reviews: %+v", cached.Reviews)
	}
}

func TestRunReportTarget(t *testing.T) {
	s := &stubInspect{
		runInfo:   model.RunInfo{Owner: "o", Repo: "r", RunID: 123},
		runFailed: []model.JobResult{failedJob()},
		runCancel: []model.JobResult{{
			ID: 7, Conclusion: "cancelled",
			Steps: []model.StepOverview{{Number: 1, Name: "Run e2e", Conclusion: "cancelled"}},
		}},
		jobLog: failLog,
	}
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", RunID: 123}

	report, err := inspectWith(context.Background(), tgt, options{
		reviewCommentLimit: 5, context: 10, shortThreshold: 100, tail: 100,
	})
	if err != nil {
		t.Fatalf("inspectWith: %v", err)
	}
	if s.runCalls != 1 {
		t.Errorf("RunReport calls = %d, want 1", s.runCalls)
	}
	if report.Run == nil || report.Run.RunID != 123 {
		t.Errorf("run info missing: %+v", report.Run)
	}
	// Both failed and cancelled jobs are drilled (2 JobLog calls).
	if s.jobLogCalls != 2 {
		t.Errorf("JobLog calls = %d, want 2 (failed + cancelled)", s.jobLogCalls)
	}
	if len(report.FailedJobs[0].FailedSteps) == 0 {
		t.Errorf("failed run job should be drilled")
	}
}

func TestRunReportError(t *testing.T) {
	s := &stubInspect{runErr: errors.New("no run")}
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", RunID: 123}
	if _, err := inspectWith(context.Background(), tgt, options{
		reviewCommentLimit: 5, context: 10, shortThreshold: 100, tail: 100,
	}); err == nil {
		t.Fatal("expected RunReport error")
	}
}

func TestRunReportWithJobID(t *testing.T) {
	s := &stubInspect{
		runInfo:   model.RunInfo{Owner: "o", Repo: "r", RunID: 123, JobID: 456},
		runFailed: []model.JobResult{failedJob()},
		jobLog:    failLog,
	}
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", RunID: 123, JobID: 456}
	report, err := inspectWith(context.Background(), tgt, options{
		reviewCommentLimit: 5, context: 10, shortThreshold: 100, tail: 100,
	})
	if err != nil {
		t.Fatalf("inspectWith: %v", err)
	}
	if report.Run == nil || report.Run.JobID != 456 {
		t.Errorf("single-job run info wrong: %+v", report.Run)
	}
}

func TestInspectExported(t *testing.T) {
	s := ciStub()
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", Number: 42}
	report, err := Inspect(context.Background(), tgt, InspectOptions{
		ReviewCommentLimit: 5, CIOnly: true, Context: 10, ShortThreshold: 100, Tail: 100,
	})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(report.FailedJobs) != 1 {
		t.Errorf("Inspect failed jobs = %d, want 1", len(report.FailedJobs))
	}
}

func TestVersionExported(t *testing.T) {
	if Version() == "" {
		t.Error("Version() returned empty")
	}
}
