package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/model"
)

var now = time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

// baseState is a target already settled on a head commit with a known review
// fingerprint — the state a poller is in most of the time, and the one from
// which the interesting transitions start.
func baseState() prState {
	return prState{
		Target:            "o/r#7",
		Owner:             "o",
		Repo:              "r",
		Number:            7,
		HeadSHA:           "abc1234def",
		Announced:         true,
		Verdict:           "passed",
		Lifecycle:         "open",
		ReviewFingerprint: "fp-1",
		ReviewsSince:      now.Add(-time.Hour),
		CommentsSince:     now.Add(-time.Hour),
	}
}

func openPR(sha string) model.PR {
	return model.PR{
		Owner: "o", Repo: "r", Number: 7,
		Title: "Add the thing", HeadSHA: sha, HeadBranch: "feature",
		State: "open",
	}
}

// kinds lists an event batch's kinds, for compact assertions.
func kinds(events []Event) []Kind {
	out := make([]Kind, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func hasKind(events []Event, k Kind) *Event {
	for i := range events {
		if events[i].Kind == k {
			return &events[i]
		}
	}
	return nil
}

func TestPollQuietRoundIsCheap(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-1" // unchanged

	st, events := testPoller(c).Poll(context.Background(), baseState(), now)

	if len(events) != 0 {
		t.Fatalf("a quiet round produced %v, want nothing", kinds(events))
	}
	// The whole point of the fingerprint: an unchanged review state must not
	// cost the two REST listings.
	if c.calls("PRReviewsSince") != 0 || c.calls("PRReviewCommentsSince") != 0 {
		t.Error("an unchanged review fingerprint must not trigger the REST review listings")
	}
	if c.calls("JobLog") != 0 {
		t.Error("a round with no new failures must not download any logs")
	}
	if got := st.NextPoll.Sub(now); got != IdleInterval {
		t.Errorf("next poll in %s, want the idle interval %s", got, IdleInterval)
	}
}

func TestPollNewCommitResetsCIState(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("newsha0000")
	c.fingerprint = "fp-1"
	c.running = []model.RunningJob{{Name: "test"}, {Name: "lint"}}

	before := baseState()
	before.ReportedJobs = []string{"1/1"}

	st, events := testPoller(c).Poll(context.Background(), before, now)

	if e := hasKind(events, KindCIStarted); e == nil {
		t.Fatalf("a new head commit with running jobs should announce itself, got %v", kinds(events))
	} else if !strings.Contains(e.Title, "newsha0") {
		t.Errorf("title %q should name the new head commit", e.Title)
	}
	if st.Verdict != "" {
		t.Errorf("Verdict = %q, want it cleared by the new commit", st.Verdict)
	}
	if len(st.ReportedJobs) != 0 {
		t.Errorf("ReportedJobs = %v, want cleared by the new commit", st.ReportedJobs)
	}
	if got := st.NextPoll.Sub(now); got != ActiveInterval {
		t.Errorf("next poll in %s, want the active interval %s while checks run", got, ActiveInterval)
	}
}

// TestPollCIStartedIsAnnouncedOnce guards the noise budget: a run that takes
// ten polls to finish must say "checks running" once, not ten times.
func TestPollCIStartedIsAnnouncedOnce(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("newsha0000")
	c.fingerprint = "fp-1"
	c.running = []model.RunningJob{{Name: "test"}}
	p := testPoller(c)

	st, events := p.Poll(context.Background(), baseState(), now)
	if hasKind(events, KindCIStarted) == nil {
		t.Fatal("first sighting should announce")
	}
	_, events = p.Poll(context.Background(), st, now.Add(time.Minute))
	if hasKind(events, KindCIStarted) != nil {
		t.Errorf("second poll announced again: %v", kinds(events))
	}
}

const failingLog = `2026-07-24T12:00:00.0000000Z ##[group]Run make test
2026-07-24T12:00:00.0000000Z make test
2026-07-24T12:00:00.0000000Z ##[endgroup]
2026-07-24T12:00:01.0000000Z --- FAIL: TestThing (0.00s)
2026-07-24T12:00:01.0000000Z     thing_test.go:42: got 1, want 2
2026-07-24T12:00:02.0000000Z ##[error]Process completed with exit code 1.
`

func failedJob(id int64, name string) model.JobResult {
	return model.JobResult{
		ID: id, RunID: 99, Name: name, Conclusion: "failure", RunAttempt: 1,
		Steps: []model.StepOverview{
			{Number: 1, Name: "Run make test", Status: "completed", Conclusion: "failure"},
		},
	}
}

func TestPollFailedJobCarriesItsLogs(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-1"
	c.failed = []model.JobResult{failedJob(11, "test")}
	c.jobLog = failingLog

	before := baseState()
	before.Verdict = ""

	st, events := testPoller(c).Poll(context.Background(), before, now)

	e := hasKind(events, KindCIFailed)
	if e == nil {
		t.Fatalf("a failed job should produce a ci.failed event, got %v", kinds(events))
	}
	if !strings.Contains(e.Title, "test failed") {
		t.Errorf("title = %q, want it to name the job and say it failed", e.Title)
	}
	// The body is the whole value of the event: an agent must be able to act on
	// it without a second call.
	if !strings.Contains(e.Body, "TestThing") || !strings.Contains(e.Body, "thing_test.go:42") {
		t.Errorf("body does not carry the failing test's output:\n%s", e.Body)
	}
	if !strings.Contains(e.URL, "/actions/runs/99/job/11") {
		t.Errorf("URL = %q, want a link to the job", e.URL)
	}
	if st.Verdict != "failed" {
		t.Errorf("Verdict = %q, want failed", st.Verdict)
	}
	if e.Severity() != SeverityAction {
		t.Error("a CI failure must be actionable so the Stop hook picks it up")
	}
}

func TestPollFailedJobReportedOncePerAttempt(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-1"
	c.failed = []model.JobResult{failedJob(11, "test")}
	c.jobLog = failingLog
	p := testPoller(c)

	before := baseState()
	before.Verdict = ""
	st, events := p.Poll(context.Background(), before, now)
	if hasKind(events, KindCIFailed) == nil {
		t.Fatal("first poll should report the failure")
	}

	_, events = p.Poll(context.Background(), st, now.Add(time.Minute))
	if hasKind(events, KindCIFailed) != nil {
		t.Errorf("the same failed attempt was reported twice: %v", kinds(events))
	}
	if c.calls("JobLog") != 1 {
		t.Errorf("JobLog called %d times, want 1 — an already-reported failure must not be re-downloaded", c.calls("JobLog"))
	}

	// A re-run is a new attempt, and does deserve a fresh report.
	c.failed = []model.JobResult{failedJob(11, "test")}
	c.failed[0].RunAttempt = 2
	if _, events = p.Poll(context.Background(), st, now.Add(2*time.Minute)); hasKind(events, KindCIFailed) == nil {
		t.Error("a re-run attempt should be reported again")
	}
}

func TestPollUnreadableLogStillReportsTheFailure(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-1"
	c.failed = []model.JobResult{failedJob(11, "test")}
	c.jobLogErr = errors.New("410 gone")

	before := baseState()
	before.Verdict = ""
	_, events := testPoller(c).Poll(context.Background(), before, now)

	e := hasKind(events, KindCIFailed)
	if e == nil {
		t.Fatal("knowing a job failed matters even when the log is out of reach")
	}
	if !strings.Contains(e.Body, "logs unavailable") {
		t.Errorf("body = %q, want it to say the logs could not be fetched", e.Body)
	}
}

// TestPollGreenVerdictClosesTheLoop covers the event the whole push/watch/fix
// loop turns on. Nothing in the API says "this commit is green" — ListJobs
// returns only failed, cancelled and running jobs — so a pass is inferred from
// having watched checks run and then stop.
func TestPollGreenVerdictClosesTheLoop(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("newsha0000")
	c.fingerprint = "fp-1"
	c.running = []model.RunningJob{{Name: "test"}}
	p := testPoller(c)

	// The push registers: checks are in flight.
	st, events := p.Poll(context.Background(), baseState(), now)
	if hasKind(events, KindCIStarted) == nil {
		t.Fatalf("expected the run to announce itself, got %v", kinds(events))
	}

	// They finish, and none of them failed.
	c.running = nil
	st, events = p.Poll(context.Background(), st, now.Add(time.Minute))
	e := hasKind(events, KindCIPassed)
	if e == nil {
		t.Fatalf("expected the green verdict, got %v", kinds(events))
	}
	if !strings.Contains(e.Title, "all checks passed") || !strings.Contains(e.Title, "newsha0") {
		t.Errorf("title = %q, want the verdict and the commit", e.Title)
	}
	if e.Severity() != SeverityInfo {
		t.Error("a passing build must never be the thing that delays an agent from finishing")
	}
	if st.Verdict != "passed" {
		t.Errorf("Verdict = %q, want passed", st.Verdict)
	}

	// And it fires once, not on every poll of a green PR.
	_, events = p.Poll(context.Background(), st, now.Add(2*time.Minute))
	if hasKind(events, KindCIPassed) != nil {
		t.Errorf("the green verdict repeated: %v", kinds(events))
	}
}

// TestPollSaysNothingAboutACommitItNeverWatched is the other half of the rule:
// a commit whose checks were already finished when the watch began is a fact,
// not news, and inferring "passed" from silence would be a lie.
func TestPollSaysNothingAboutACommitItNeverWatched(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-1"

	before := baseState()
	before.Verdict = ""
	before.Announced = false

	_, events := testPoller(c).Poll(context.Background(), before, now)
	if len(events) != 0 {
		t.Fatalf("a commit with no checks ever seen should stay quiet, got %v", kinds(events))
	}
}

// TestPollFailureSuppressesTheGreenVerdict guards against reporting a pass in
// the same round a job went red.
func TestPollFailureSuppressesTheGreenVerdict(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-1"
	c.failed = []model.JobResult{failedJob(11, "test")}
	c.jobLog = failingLog

	before := baseState()
	before.Verdict = ""
	before.Announced = true

	_, events := testPoller(c).Poll(context.Background(), before, now)
	if hasKind(events, KindCIPassed) != nil {
		t.Errorf("a round with a failure must not also report a pass: %v", kinds(events))
	}
	if hasKind(events, KindCIFailed) == nil {
		t.Errorf("expected the failure, got %v", kinds(events))
	}
}

func TestPollNonActionsCheckFailure(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-1"
	c.other = []model.OtherCheck{{Name: "codecov/patch", Conclusion: "failure", URL: "https://codecov.io/x"}}

	before := baseState()
	before.Verdict = ""
	st, events := testPoller(c).Poll(context.Background(), before, now)

	e := hasKind(events, KindCIFailed)
	if e == nil {
		t.Fatalf("a red non-Actions check should fail the verdict, got %v", kinds(events))
	}
	if !strings.Contains(e.Body, "codecov/patch") || !strings.Contains(e.Body, "https://codecov.io/x") {
		t.Errorf("body should name the check and link to it:\n%s", e.Body)
	}
	if st.Verdict != "failed" {
		t.Errorf("Verdict = %q, want failed", st.Verdict)
	}
}

func TestPollLifecycleChange(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.pr.State = "closed"
	c.pr.Merged = true
	c.fingerprint = "fp-1"

	st, events := testPoller(c).Poll(context.Background(), baseState(), now)

	e := hasKind(events, KindPRState)
	if e == nil {
		t.Fatalf("a merge should be reported, got %v", kinds(events))
	}
	if !strings.Contains(e.Title, "merged") {
		t.Errorf("title = %q, want it to say merged", e.Title)
	}
	if got := st.NextPoll.Sub(now); got != DormantInterval {
		t.Errorf("next poll in %s, want the dormant interval %s for a merged PR", got, DormantInterval)
	}
}

// TestPollFirstSightingIsNotNews covers the rule that keeps a session from
// being handed a PR's whole history the moment it starts watching.
func TestPollFirstSightingIsNotNews(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-1"
	c.reviews = []gh.PRReview{{ID: 1, State: "CHANGES_REQUESTED", UserLogin: "alice", SubmittedAt: now.Add(-24 * time.Hour)}}
	c.comments = []gh.PRReviewComment{{ID: 5, Path: "a.go", Line: 3, Body: "old", CreatedAt: now.Add(-24 * time.Hour)}}

	fresh := prState{Target: "o/r#7", Owner: "o", Repo: "r", Number: 7}
	st, events := testPoller(c).Poll(context.Background(), fresh, now)

	if hasKind(events, KindReviewSubmitted) != nil || hasKind(events, KindReviewComment) != nil {
		t.Fatalf("the first sighting replayed history: %v", kinds(events))
	}
	if st.ReviewFingerprint != "fp-1" {
		t.Errorf("fingerprint = %q, want it recorded", st.ReviewFingerprint)
	}
	if !st.CommentsSince.Equal(now) {
		t.Errorf("CommentsSince = %v, want the high-water mark set to now", st.CommentsSince)
	}
}

func TestPollNewReviewComment(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-2" // moved
	c.file = []byte("package main\n\nfunc main() {}\n")
	c.comments = []gh.PRReviewComment{{
		ID:        5,
		Path:      "main.go",
		Line:      3,
		Side:      "RIGHT",
		Body:      "this should return an error",
		DiffHunk:  "@@ -1,3 +1,3 @@\n func main() {}",
		CommitID:  "abc1234def",
		UserLogin: "alice",
		CreatedAt: now.Add(-time.Minute),
	}}

	_, events := testPoller(c).Poll(context.Background(), baseState(), now)

	e := hasKind(events, KindReviewComment)
	if e == nil {
		t.Fatalf("a new comment should be reported, got %v", kinds(events))
	}
	if !strings.Contains(e.Title, "alice") || !strings.Contains(e.Title, "main.go:3") {
		t.Errorf("title = %q, want the reviewer and the anchor", e.Title)
	}
	for _, want := range []string{"this should return an error", "Diff hunk:", "func main()"} {
		if !strings.Contains(e.Body, want) {
			t.Errorf("body is missing %q — an agent should not need a second call:\n%s", want, e.Body)
		}
	}
	if !strings.Contains(e.URL, "#discussion_r5") {
		t.Errorf("URL = %q, want a link to the comment", e.URL)
	}
}

func TestPollReplyCarriesItsThread(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-2"
	c.thread = []gh.PRReviewComment{
		{ID: 4, UserLogin: "bob", Body: "why not a switch here?"},
		{ID: 5, UserLogin: "alice", Body: "because of the fallthrough"},
	}
	c.comments = []gh.PRReviewComment{{
		ID: 5, InReplyTo: 4, Path: "main.go", Line: 3, Side: "RIGHT",
		Body: "because of the fallthrough", UserLogin: "alice", CreatedAt: now.Add(-time.Minute),
	}}

	_, events := testPoller(c).Poll(context.Background(), baseState(), now)

	e := hasKind(events, KindReviewComment)
	if e == nil {
		t.Fatal("expected a review comment event")
	}
	if !strings.Contains(e.Body, "why not a switch here?") {
		t.Errorf("a reply must carry what it replies to:\n%s", e.Body)
	}
}

func TestPollNewCommentReportedOnce(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-2"
	c.comments = []gh.PRReviewComment{{
		ID: 5, Path: "main.go", Line: 3, Side: "RIGHT", Body: "fix", UserLogin: "alice",
		CreatedAt: now.Add(-time.Minute),
	}}
	p := testPoller(c)

	st, events := p.Poll(context.Background(), baseState(), now)
	if hasKind(events, KindReviewComment) == nil {
		t.Fatal("first poll should report the comment")
	}

	// The fingerprint moves again (a thread got resolved, say) but the comment
	// itself is not new. GitHub's `since` filter is inclusive, so without the
	// reported-id guard this would repeat.
	c.fingerprint = "fp-3"
	_, events = p.Poll(context.Background(), st, now.Add(time.Minute))
	if hasKind(events, KindReviewComment) != nil {
		t.Errorf("the same comment was reported twice: %v", kinds(events))
	}
}

func TestPollSubmittedReview(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-2"
	c.reviews = []gh.PRReview{{
		ID: 1, State: "CHANGES_REQUESTED", Body: "a few things", UserLogin: "alice",
		SubmittedAt: now.Add(-time.Minute),
	}}

	_, events := testPoller(c).Poll(context.Background(), baseState(), now)

	e := hasKind(events, KindReviewSubmitted)
	if e == nil {
		t.Fatalf("a submitted review should be reported, got %v", kinds(events))
	}
	if !strings.Contains(e.Title, "alice requested changes") {
		t.Errorf("title = %q, want the reviewer and verdict", e.Title)
	}
	if !strings.Contains(e.Body, "a few things") {
		t.Errorf("body should carry the review body:\n%s", e.Body)
	}
}

func TestPollErrorIsReportedOnceThenBacksOff(t *testing.T) {
	c := newFakeClient()
	c.prErr = errors.New("401 bad credentials")
	p := testPoller(c)

	st, events := p.Poll(context.Background(), baseState(), now)
	if hasKind(events, KindError) == nil {
		t.Fatalf("the first failure should be reported, got %v", kinds(events))
	}
	if st.Failures != 1 {
		t.Errorf("Failures = %d, want 1", st.Failures)
	}

	st2, events := p.Poll(context.Background(), st, now.Add(time.Minute))
	if len(events) != 0 {
		t.Errorf("the same failure was reported again: %v", kinds(events))
	}
	if st2.NextPoll.Sub(now.Add(time.Minute)) <= st.NextPoll.Sub(now) {
		t.Error("repeated failures should lengthen the backoff")
	}

	// A different error is news again.
	c.prErr = errors.New("404 not found")
	if _, events = p.Poll(context.Background(), st2, now.Add(2*time.Minute)); hasKind(events, KindError) == nil {
		t.Error("a different error should be reported")
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	if got := backoff(1); got != ActiveInterval {
		t.Errorf("backoff(1) = %s, want %s", got, ActiveInterval)
	}
	if backoff(2) <= backoff(1) {
		t.Error("backoff should grow")
	}
	if got := backoff(20); got != MaxBackoff {
		t.Errorf("backoff(20) = %s, want it capped at %s", got, MaxBackoff)
	}
}

func TestPollStretchesWhenQuotaIsLow(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-1"
	c.rateRemaining = LowRateThreshold - 1

	st, _ := testPoller(c).Poll(context.Background(), baseState(), now)
	if got := st.NextPoll.Sub(now); got != 2*IdleInterval {
		t.Errorf("next poll in %s, want the interval doubled (%s) on a low quota", got, 2*IdleInterval)
	}
}

func TestPollDegradesWhenSubCallsFail(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.jobsErr = errors.New("500")
	c.fingerprintErr = errors.New("graphql down")

	st, events := testPoller(c).Poll(context.Background(), baseState(), now)

	// The round produced nothing, but it did not fail: the PR itself was
	// readable, so the target is healthy and stays on its normal cadence.
	if len(events) != 0 {
		t.Errorf("degraded sub-calls should stay quiet, got %v", kinds(events))
	}
	if st.Failures != 0 {
		t.Errorf("Failures = %d, want 0 — only a failed GetPR counts against the target", st.Failures)
	}
}

func TestJobNamesCaps(t *testing.T) {
	running := []model.RunningJob{{Name: "d"}, {Name: "c"}, {Name: "b"}, {Name: "a"}, {Name: "e"}}
	got := jobNames(running)
	if !strings.Contains(got, "and 2 more") {
		t.Errorf("jobNames = %q, want the tail collapsed", got)
	}
	if !strings.HasPrefix(got, "a, b, c") {
		t.Errorf("jobNames = %q, want the names sorted", got)
	}
	if got := jobNames([]model.RunningJob{{Name: "only"}}); got != "only" {
		t.Errorf("jobNames = %q, want %q", got, "only")
	}
}

func TestShortSHAAndConclusionVerb(t *testing.T) {
	if got := shortSHA("0123456789"); got != "0123456" {
		t.Errorf("shortSHA = %q", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("shortSHA = %q, want a short SHA left alone", got)
	}
	if got := conclusionVerb("cancelled"); got != "was cancelled" {
		t.Errorf("conclusionVerb = %q", got)
	}
	if got := conclusionVerb("failure"); got != "failed" {
		t.Errorf("conclusionVerb = %q", got)
	}
}

func TestVerdictPhrase(t *testing.T) {
	for verdict, want := range map[string]string{
		"approved":          "approved",
		"changes_requested": "requested changes",
		"commented":         "commented",
		"dismissed":         "commented",
	} {
		if got := verdictPhrase(verdict); got != want {
			t.Errorf("verdictPhrase(%q) = %q, want %q", verdict, got, want)
		}
	}
}
