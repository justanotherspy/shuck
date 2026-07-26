package monitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

// kindsOf collects every event of one kind. Batching makes the count the point
// of most assertions here — "one event for this run", not "an event exists".
func kindsOf(events []Event, k Kind) []Event {
	var out []Event
	for _, e := range events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

// runJob builds a failed job belonging to a particular run, finishing `after`
// into it. The timings are what decide whether it may interrupt or has to wait.
func runJob(id, runID int64, name string, after time.Duration) model.JobResult {
	j := failedJob(id, name)
	j.RunID = runID
	j.WorkflowName = "CI"
	j.RunStartedAt = now
	j.CompletedAt = now.Add(after)
	return j
}

func runningJob(runID int64, name string) model.RunningJob {
	return model.RunningJob{
		Name: name, Status: "in_progress", WorkflowName: "CI",
		RunID: runID, RunAttempt: 1, RunStartedAt: now,
	}
}

// pollFor drives one round against a fake whose CI state has been set up.
func pollFor(t *testing.T, c *fakeClient, st prState, at time.Time) (prState, []Event) {
	t.Helper()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-1"
	return testPoller(c).Poll(context.Background(), st, at)
}

// ciState is a state that has already seen this commit's checks in flight, and
// holds no verdict — the state a run is polled in while it is going.
func ciState() prState {
	st := baseState()
	st.Verdict = ""
	return st
}

// TestRunFailuresWaitForTheRestOfTheirRun is the batching rule. A run reported
// job-by-job is a run an agent fixes job-by-job: it reads the first failure,
// starts work, and is interrupted by the second.
func TestRunFailuresWaitForTheRestOfTheirRun(t *testing.T) {
	c := newFakeClient()
	// Both failed well past the fast-fail window, so neither may jump the queue.
	c.failed = []model.JobResult{
		runJob(1, 99, "test", 5*time.Minute),
		runJob(2, 99, "build", 6*time.Minute),
	}
	c.running = []model.RunningJob{runningJob(99, "docs")}

	st, events := pollFor(t, c, ciState(), now.Add(7*time.Minute))
	if len(events) != 0 {
		t.Fatalf("a run still in flight reported early: %v", kinds(events))
	}

	// The last job finishes; now the run has something complete to say.
	c.running = nil
	_, events = pollFor(t, c, st, now.Add(8*time.Minute))

	failures := kindsOf(events, KindCIFailed)
	if len(failures) != 1 {
		t.Fatalf("want one event for the whole run, got %d: %v", len(failures), kinds(events))
	}
	e := failures[0]
	if !strings.Contains(e.Title, "2 jobs failed") {
		t.Errorf("title should count the run's failures, got %q", e.Title)
	}
	for _, want := range []string{"test", "build"} {
		if !strings.Contains(e.Body, want) {
			t.Errorf("body is missing job %q:\n%s", want, e.Body)
		}
	}
}

// TestEarlyFailureDoesNotWait is the exception that keeps batching from costing
// the monitor its point. A lint job that fails in forty seconds must not sit
// behind a six-minute sibling.
func TestEarlyFailureDoesNotWait(t *testing.T) {
	c := newFakeClient()
	c.failed = []model.JobResult{runJob(1, 99, "lint", 40*time.Second)}
	c.running = []model.RunningJob{runningJob(99, "test")}

	st, events := pollFor(t, c, ciState(), now.Add(time.Minute))

	failures := kindsOf(events, KindCIFailed)
	if len(failures) != 1 {
		t.Fatalf("an early failure should report at once, got %v", kinds(events))
	}
	if !strings.Contains(failures[0].Title, "lint failed") {
		t.Errorf("title = %q, want the job named", failures[0].Title)
	}
	// It must say the run is not over, or the agent reads one failure as the
	// whole verdict.
	if !strings.Contains(failures[0].Body, "still running") {
		t.Errorf("an escalated failure must say the run is unfinished:\n%s", failures[0].Body)
	}

	// The slow sibling fails later: a second event, and the first is not repeated.
	c.failed = append(c.failed, runJob(2, 99, "test", 6*time.Minute))
	c.running = nil
	_, events = pollFor(t, c, st, now.Add(7*time.Minute))

	failures = kindsOf(events, KindCIFailed)
	if len(failures) != 1 {
		t.Fatalf("want exactly the newly-failed job, got %v", kinds(events))
	}
	if strings.Contains(failures[0].Title, "lint") {
		t.Errorf("the already-reported job was reported again: %q", failures[0].Title)
	}
}

// TestSlowRunDoesNotHoldAnotherRunsFailures is why batching is scoped to a run
// rather than to the commit. CodeQL taking six minutes is not a reason to sit
// on CI's failures for six minutes.
func TestSlowRunDoesNotHoldAnotherRunsFailures(t *testing.T) {
	c := newFakeClient()
	c.failed = []model.JobResult{runJob(1, 99, "test", 5*time.Minute)}
	// Run 99 is done; run 100 (a different workflow) is still going.
	c.running = []model.RunningJob{runningJob(100, "Analyze")}

	_, events := pollFor(t, c, ciState(), now.Add(6*time.Minute))

	failures := kindsOf(events, KindCIFailed)
	if len(failures) != 1 {
		t.Fatalf("a finished run's failure was held by an unrelated one: %v", kinds(events))
	}
	if strings.Contains(failures[0].Body, "still running") {
		t.Errorf("run 99 is complete; its event should not claim otherwise:\n%s", failures[0].Body)
	}
}

// TestCancelledJobsAreANoteNotAnEvent is the fix for the loudest version of the
// old behavior: fail-fast cancels every sibling of the job that failed, and
// each cancellation used to arrive as its own red event.
func TestCancelledJobsAreANoteNotAnEvent(t *testing.T) {
	c := newFakeClient()
	c.failed = []model.JobResult{runJob(1, 99, "test", 2*time.Minute)}
	cancelled := runJob(2, 99, "build", 2*time.Minute)
	cancelled.Conclusion = "cancelled"
	c.cancelled = []model.JobResult{cancelled}

	_, events := pollFor(t, c, ciState(), now.Add(3*time.Minute))

	failures := kindsOf(events, KindCIFailed)
	if len(failures) != 1 {
		t.Fatalf("want one event for the run, got %d: %v", len(failures), kinds(events))
	}
	if !strings.Contains(failures[0].Title, "test failed") {
		t.Errorf("the cancellation was counted as a failure: %q", failures[0].Title)
	}
	if !strings.Contains(failures[0].Body, "cancelled") || !strings.Contains(failures[0].Body, "build") {
		t.Errorf("the cancelled job should be noted by name:\n%s", failures[0].Body)
	}
}

// TestCancelledWithoutFailuresIsNotCalledPassing covers the claim that would be
// most dangerous to get wrong: a cancelled job is a check that never ran, so
// "all checks passed" over the top of one is a false all-clear.
func TestCancelledWithoutFailuresIsNotCalledPassing(t *testing.T) {
	c := newFakeClient()
	cancelled := runJob(2, 99, "build", 2*time.Minute)
	cancelled.Conclusion = "cancelled"
	c.cancelled = []model.JobResult{cancelled}

	_, events := pollFor(t, c, ciState(), now.Add(3*time.Minute))

	e := hasKind(events, KindCIPassed)
	if e == nil {
		t.Fatalf("a finished commit should still report a verdict, got %v", kinds(events))
	}
	if strings.Contains(e.Title, "all checks passed") {
		t.Errorf("a cancelled job was reported as a pass: %q", e.Title)
	}
	if !strings.Contains(e.Title, "cancelled") {
		t.Errorf("the verdict should say what was cancelled: %q", e.Title)
	}
	if !strings.Contains(e.Body, "build") {
		t.Errorf("the verdict should name the unverified job:\n%s", e.Body)
	}
	if e.Severity() != SeverityInfo {
		t.Error("a cancellation is not something to hold a turn open over")
	}
}

// TestStalledRunReportsWhatItHas is the escape hatch. Without it a job that
// hangs holds its run's failures forever, which is worse than reporting them
// late — it reports them never.
func TestStalledRunReportsWhatItHas(t *testing.T) {
	c := newFakeClient()
	c.failed = []model.JobResult{runJob(1, 99, "test", 5*time.Minute)}
	c.running = []model.RunningJob{runningJob(99, "wedged")}

	// Inside the stall window the failure waits.
	st, events := pollFor(t, c, ciState(), now.Add(10*time.Minute))
	if len(events) != 0 {
		t.Fatalf("a run that is merely slow should still be waited for: %v", kinds(events))
	}

	// Past it, what is known goes out.
	_, events = pollFor(t, c, st, now.Add(runStallAfter+time.Minute))
	failures := kindsOf(events, KindCIFailed)
	if len(failures) != 1 {
		t.Fatalf("a stalled run never reported its failure: %v", kinds(events))
	}
	if !strings.Contains(failures[0].Body, "still running") {
		t.Errorf("a stalled run's event must say what is still pending:\n%s", failures[0].Body)
	}
}

// TestUnknownTimingsFallBackToWaiting pins the direction the escalation fails
// in. Without timings shuck cannot tell an early failure from a late one, and
// guessing "early" would reintroduce piecemeal reporting on every run whose
// timings the API withheld.
func TestUnknownTimingsFallBackToWaiting(t *testing.T) {
	c := newFakeClient()
	j := runJob(1, 99, "test", time.Second)
	j.RunStartedAt, j.CompletedAt = time.Time{}, time.Time{}
	c.failed = []model.JobResult{j}
	c.running = []model.RunningJob{runningJob(99, "build")}

	st, events := pollFor(t, c, ciState(), now.Add(time.Minute))
	if len(events) != 0 {
		t.Fatalf("a job with no timings should wait for its run: %v", kinds(events))
	}

	c.running = nil
	_, events = pollFor(t, c, st, now.Add(2*time.Minute))
	if len(kindsOf(events, KindCIFailed)) != 1 {
		t.Fatalf("the failure should arrive when the run finishes: %v", kinds(events))
	}
}

// TestReRunIsJudgedOnItsOwnAttempt guards the grouping key. A re-run reuses the
// run id, so grouping on the id alone would let the first attempt's reported
// jobs suppress the second attempt's.
func TestReRunIsJudgedOnItsOwnAttempt(t *testing.T) {
	c := newFakeClient()
	c.failed = []model.JobResult{runJob(1, 99, "test", 5*time.Minute)}

	st, events := pollFor(t, c, ciState(), now.Add(6*time.Minute))
	if len(kindsOf(events, KindCIFailed)) != 1 {
		t.Fatalf("the first attempt should report, got %v", kinds(events))
	}

	// Re-run: same run and job ids, second attempt, red again.
	again := runJob(1, 99, "test", 5*time.Minute)
	again.RunAttempt = 2
	c.failed = []model.JobResult{again}

	_, events = pollFor(t, c, st, now.Add(12*time.Minute))
	if len(kindsOf(events, KindCIFailed)) != 1 {
		t.Fatalf("a re-run that failed again must report again, got %v", kinds(events))
	}
}
