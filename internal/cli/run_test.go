package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/cache"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/target"
)

func TestSleepCtx(t *testing.T) {
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Error("sleepCtx should report true after the full duration elapsed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Error("sleepCtx should report false when ctx is already cancelled")
	}
}

func TestRunWatchValidation(t *testing.T) {
	var out, errb strings.Builder
	tgt := target.Target{Owner: "o", Repo: "r", Number: 1}

	if _, err := runWatch(context.Background(), tgt,
		options{watch: true, interval: 0}, &out, &errb); err == nil {
		t.Error("--interval <= 0 should error")
	}
	if _, err := runWatch(context.Background(), tgt,
		options{watch: true, interval: time.Second, watchTimeout: -time.Second}, &out, &errb); err == nil {
		t.Error("negative --watch-timeout should error")
	}
}

// TestRunWatchHappyPath drives runWatch through to a terminal report using the
// stubbed inspect + security clients, so it returns immediately without sleeping.
func TestRunWatchHappyPath(t *testing.T) {
	s := ciStub() // failed jobs, no running -> terminal report
	withStubInspect(t, s)
	withStubSecurity(t, okStub())
	// withStubSecurity clears the token withStubInspect set; restore it.
	t.Setenv("GITHUB_TOKEN", "test-token")

	var out, errb strings.Builder
	tgt := target.Target{Owner: "o", Repo: "r", Number: 42}
	code, err := runWatch(context.Background(), tgt,
		options{watch: true, interval: time.Second, reviewCommentLimit: 5,
			context: 10, shortThreshold: 100, tail: 100, state: "open"}, &out, &errb)
	if err != nil {
		t.Fatalf("runWatch: %v", err)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1 (failures present)", code)
	}
	if !strings.Contains(out.String(), "build") {
		t.Errorf("expected the report on stdout, got %q", out.String())
	}
}

func TestLoadOfflineErrors(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())

	if _, err := loadOffline(target.Target{Owner: "o", Repo: "r"}); err == nil {
		t.Error("offline without a PR number should error")
	}
	if _, err := loadOffline(target.Target{Owner: "o", Repo: "r", Number: 99}); err == nil {
		t.Error("offline with no cache should error")
	}
}

func TestInspectWithOfflineRunIDRejected(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	tgt := target.Target{Owner: "o", Repo: "r", RunID: 5}
	if _, err := inspectWith(context.Background(), tgt, options{
		offline: true, reviewCommentLimit: 5, context: 10, shortThreshold: 100, tail: 100,
	}); err == nil {
		t.Error("offline + run target should be rejected")
	}
}

// TestInspectWithOfflineFocus seeds a cache holding both CI and reviews, then
// proves the offline focus modes narrow the rendered report.
func TestInspectWithOfflineFocus(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	report := &model.Report{
		PR:         model.PR{Owner: "o", Repo: "r", Number: 42, HeadSHA: "abc1234"},
		FailedJobs: []model.JobResult{failedJob()},
		Reviews:    []model.Review{{Author: "alice", State: "approved"}},
	}
	report.ReviewsFingerprint = "fp"
	if err := cache.Save(report); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	tgt := target.Target{Owner: "o", Repo: "r", Number: 42}
	base := options{offline: true, reviewCommentLimit: 5, context: 10, shortThreshold: 100, tail: 100}

	ci := base
	ci.ciOnly = true
	got, err := inspectWith(context.Background(), tgt, ci)
	if err != nil {
		t.Fatalf("offline ci-only: %v", err)
	}
	if len(got.Reviews) != 0 {
		t.Errorf("ci-only offline should drop reviews, got %+v", got.Reviews)
	}
	if len(got.FailedJobs) != 1 {
		t.Errorf("ci-only offline should keep CI, got %+v", got.FailedJobs)
	}

	rev := base
	rev.reviewsOnly = true
	got, err = inspectWith(context.Background(), tgt, rev)
	if err != nil {
		t.Fatalf("offline reviews-only: %v", err)
	}
	if len(got.FailedJobs) != 0 {
		t.Errorf("reviews-only offline should drop CI, got %+v", got.FailedJobs)
	}
	if len(got.Reviews) != 1 {
		t.Errorf("reviews-only offline should keep reviews, got %+v", got.Reviews)
	}
}

// TestRunDefaultEndToEnd drives Run([]string{"o/r","42"}) through inspectAll /
// emitAll / withSecurity with both clients stubbed, in text and JSON form.
func TestRunDefaultEndToEnd(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		name := "text"
		args := []string{"o/r", "42"}
		if jsonOut {
			name = "json"
			args = append(args, "--json")
		}
		t.Run(name, func(t *testing.T) {
			s := ciStub()
			withStubInspect(t, s)
			withStubSecurity(t, okStub())
			t.Setenv("GITHUB_TOKEN", "test-token")

			var out, errb strings.Builder
			code := Run(args, &out, &errb)
			if code != 1 {
				t.Fatalf("exit = %d, want 1; stderr=%q", code, errb.String())
			}
			got := out.String()
			if !strings.Contains(got, "build") {
				t.Errorf("missing CI output:\n%s", got)
			}
			if jsonOut && !strings.Contains(got, "\"inspection\"") {
				t.Errorf("--json should emit the combined envelope:\n%s", got)
			}
			if !jsonOut && !strings.Contains(got, "security alerts") {
				t.Errorf("text output should include the security section:\n%s", got)
			}
		})
	}
}

func TestRunLogsSubcommandEndToEnd(t *testing.T) {
	s := ciStub()
	withStubInspect(t, s)

	var out, errb strings.Builder
	code := Run([]string{"logs", "o/r", "42"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "build") {
		t.Errorf("missing CI output:\n%s", out.String())
	}
	// logs is CI-only: reviews must not be fetched.
	if s.reviewsCalls != 0 || s.fpCalls != 0 {
		t.Errorf("logs subcommand should not fetch reviews: fp=%d reviews=%d", s.fpCalls, s.reviewsCalls)
	}
}

func TestRunLogsSingleRun(t *testing.T) {
	s := &stubInspect{
		runInfo:   model.RunInfo{Owner: "o", Repo: "r", RunID: 123},
		runFailed: []model.JobResult{failedJob()},
		jobLog:    failLog,
	}
	withStubInspect(t, s)

	var out, errb strings.Builder
	code := Run([]string{"logs", "--run", "123", "o/r"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, errb.String())
	}
	if s.runCalls != 1 {
		t.Errorf("RunReport calls = %d, want 1", s.runCalls)
	}
}

func TestRunImageAnonymousNoTags(t *testing.T) {
	s := &stubImageLister{} // no tags returned
	withStubImageLister(t, s)

	var out, errb strings.Builder
	if code := runImage([]string{"ghcr.io/acme/api"}, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no tags") {
		t.Errorf("expected a no-tags error, got %q", errb.String())
	}
}

func TestRunImageAuthedNoVersions(t *testing.T) {
	s := &stubImageLister{versions: map[string][]model.ImageVersion{}}
	withStubImageLister(t, s)
	t.Setenv("GITHUB_TOKEN", "x")

	var out, errb strings.Builder
	if code := runImage([]string{"ghcr.io/acme/api"}, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no published versions") {
		t.Errorf("expected a no-versions error, got %q", errb.String())
	}
}

func TestRunLogsInspectError(t *testing.T) {
	s := ciStub()
	s.prErr = errors.New("boom")
	withStubInspect(t, s)

	var out, errb strings.Builder
	if code := runLogs([]string{"o/r", "42"}, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "shuck:") {
		t.Errorf("expected a shuck: error, got %q", errb.String())
	}
}

func TestRunLogsBadTarget(t *testing.T) {
	var out, errb strings.Builder
	// An unparseable target fails before any network call.
	if code := runLogs([]string{"not a target!!"}, &out, &errb); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestRunReviewsInspectError(t *testing.T) {
	s := ciStub()
	s.fpErr = errors.New("graphql down") // reviews errors are non-fatal
	s.prErr = errors.New("boom")         // GetPR error is fatal
	withStubInspect(t, s)

	var out, errb strings.Builder
	if code := runReviews([]string{"o/r", "42"}, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%q", code, errb.String())
	}
}

func TestRunReviewsSubcommandEndToEnd(t *testing.T) {
	s := ciStub()
	s.fingerprint = "fp"
	s.reviews = []model.Review{{Author: "alice", State: "approved"}}
	withStubInspect(t, s)

	var out, errb strings.Builder
	code := Run([]string{"reviews", "o/r", "42"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (reviews-only ignores CI failures); stderr=%q", code, errb.String())
	}
	// reviews is reviews-only: jobs must not be listed.
	if s.jobsCalls != 0 {
		t.Errorf("reviews subcommand should not list jobs: ListJobs calls = %d", s.jobsCalls)
	}
	if !strings.Contains(out.String(), "alice") {
		t.Errorf("missing reviews output:\n%s", out.String())
	}
}
