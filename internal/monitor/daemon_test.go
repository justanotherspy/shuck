package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/model"
)

// newTestDaemon builds a daemon over a temp directory and a fake GitHub
// client, without listening on anything. The run loop, the retargeting, and the
// request handlers are all reachable from here, which is where the behavior
// worth testing lives; the socket is exercised separately.
func newTestDaemon(t *testing.T, c prClient) (*Daemon, *bytes.Buffer) {
	t.Helper()
	var log bytes.Buffer

	original := newPRClient
	newPRClient = func(string) prClient { return c }
	t.Cleanup(func() { newPRClient = original })

	d, err := newDaemon(t.TempDir(), Options{Log: &log, Version: "test", NoPins: true})
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	return d, &log
}

// treeAt lays out a git checkout the daemon can read and returns its path.
func treeAt(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	writeRepo(t, dir, "ref: refs/heads/"+branch+"\n", "refs/heads/"+branch, "abc\n", originConfig)
	return dir
}

func TestDaemonRetargetsToTheBranchesPR(t *testing.T) {
	c := newFakeClient()
	c.openPR = 42
	d, _ := newTestDaemon(t, c)

	dir := treeAt(t, "feature")
	d.watches.Add(Watch{ID: TreeWatchID(dir), Kind: WatchTree, Path: dir})

	d.retarget(context.Background(), now)

	w, ok := d.watches.Get(TreeWatchID(dir))
	if !ok {
		t.Fatal("the watch disappeared")
	}
	if w.Owner != "justanotherspy" || w.Repo != "shuck" || w.Branch != "feature" || w.Number != 42 {
		t.Fatalf("watch resolved to %+v, want justanotherspy/shuck#42 on feature", w)
	}
	events := d.journal.Since(0, 0)
	if len(events) != 1 || events[0].Kind != KindTarget {
		t.Fatalf("expected one watch.target event, got %d", len(events))
	}
	if !strings.Contains(events[0].Title, "now watching justanotherspy/shuck#42") {
		t.Errorf("title = %q", events[0].Title)
	}
}

// TestDaemonRetargetIsCheapWhenNothingMoved guards the tick budget: the daemon
// wakes every second, and a settled watch must cost nothing but the HEAD read.
func TestDaemonRetargetIsCheapWhenNothingMoved(t *testing.T) {
	c := newFakeClient()
	c.openPR = 42
	d, _ := newTestDaemon(t, c)

	dir := treeAt(t, "feature")
	d.watches.Add(Watch{ID: TreeWatchID(dir), Kind: WatchTree, Path: dir})

	for i := range 5 {
		d.retarget(context.Background(), now.Add(time.Duration(i)*time.Second))
	}
	if got := c.calls("FindOpenPR"); got != 1 {
		t.Errorf("FindOpenPR called %d times over five ticks, want 1", got)
	}
}

// TestDaemonUnresolvedWatchBacksOff is the same budget for the other case: a
// branch with no PR must not ask GitHub every second whether one appeared.
func TestDaemonUnresolvedWatchBacksOff(t *testing.T) {
	c := newFakeClient()
	c.openPRErr = fmt.Errorf("%w: %q", gh.ErrNoOpenPR, "feature")
	d, _ := newTestDaemon(t, c)

	dir := treeAt(t, "feature")
	d.watches.Add(Watch{ID: TreeWatchID(dir), Kind: WatchTree, Path: dir})

	for i := range 10 {
		d.retarget(context.Background(), now.Add(time.Duration(i)*time.Second))
	}
	if got := c.calls("FindOpenPR"); got != 1 {
		t.Errorf("FindOpenPR called %d times within one resolve interval, want 1", got)
	}
	// Past the interval it does ask again — a PR may have been opened.
	d.retarget(context.Background(), now.Add(ResolveInterval+time.Second))
	if got := c.calls("FindOpenPR"); got != 2 {
		t.Errorf("FindOpenPR called %d times after the resolve interval, want 2", got)
	}
	// And it says so exactly once, not once per attempt.
	targetEvents := 0
	for _, e := range d.journal.Since(0, 0) {
		if e.Kind == KindTarget {
			targetEvents++
		}
	}
	if targetEvents != 1 {
		t.Errorf("%d watch.target events for one unchanging situation, want 1", targetEvents)
	}
}

// TestDaemonLookupFailureIsNotReportedAsNoPR is a bug this found in the wild:
// a token that cannot see the repository produced "no open PR for <branch>",
// which sends you looking for a pull request instead of at your token.
func TestDaemonLookupFailureIsNotReportedAsNoPR(t *testing.T) {
	c := newFakeClient()
	c.openPRErr = errors.New("403 GitHub access is not enabled for this session")
	d, _ := newTestDaemon(t, c)

	dir := treeAt(t, "feature")
	d.watches.Add(Watch{ID: TreeWatchID(dir), Kind: WatchTree, Path: dir})
	d.retarget(context.Background(), now)

	events := d.journal.Since(0, 0)
	if len(events) != 1 {
		t.Fatalf("%d events, want 1", len(events))
	}
	if events[0].Kind != KindError {
		t.Errorf("Kind = %q, want %q — a failed lookup is a problem, not a fact about the branch", events[0].Kind, KindError)
	}
	if strings.Contains(events[0].Title, "no open PR") {
		t.Errorf("title = %q, want it to name the real failure", events[0].Title)
	}
	if !strings.Contains(events[0].Title, "403") {
		t.Errorf("title = %q, want it to carry the underlying error", events[0].Title)
	}
}

// TestDaemonPokeSurvivesAnInFlightPoll covers a race a review probe found: a
// poke that lands while its target is mid-poll was overwritten by the poll's
// own, much later, deadline — so the push you just made waited out a full
// interval anyway.
func TestDaemonPokeSurvivesAnInFlightPoll(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.watches.Add(Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7})

	due := d.due(time.Now())
	if len(due) != 1 {
		t.Fatalf("%d targets due, want 1", len(due))
	}
	inFlight := due[0]

	// A client pokes while that poll is still running.
	d.handlePoke(Request{Op: OpPoke})

	// The poll finishes and stores the deadline it computed.
	inFlight.NextPoll = time.Now().Add(IdleInterval)
	d.store(inFlight, nil)

	if got := d.targets["o/r#7"].NextPoll; got.After(time.Now().Add(time.Second)) {
		t.Errorf("next poll in %v — the poke was lost to the in-flight poll's result", time.Until(got))
	}
}

// TestDaemonRoundStopsWhenAskedTo covers the other half of that probe: a round
// working through several targets must give up when someone asks the daemon to
// stop, rather than finishing the list first.
func TestDaemonRoundStopsWhenAskedTo(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.watches.Add(Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7})
	d.Shutdown()

	d.round(context.Background(), time.Now())

	if len(d.journal.Since(0, 0)) != 0 {
		t.Error("a round after Shutdown should not have polled anything")
	}
}

func TestDaemonRetargetOnBranchSwitch(t *testing.T) {
	c := newFakeClient()
	c.openPR = 42
	d, _ := newTestDaemon(t, c)

	dir := treeAt(t, "feature")
	d.watches.Add(Watch{ID: TreeWatchID(dir), Kind: WatchTree, Path: dir})
	d.retarget(context.Background(), now)

	// Switch branches under the daemon's feet, exactly as `git switch` does.
	write(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/other\n")
	c.openPR = 43
	d.retarget(context.Background(), now.Add(time.Second))

	w, _ := d.watches.Get(TreeWatchID(dir))
	if w.Branch != "other" || w.Number != 43 {
		t.Fatalf("watch = %+v, want it retargeted to #43 on other", w)
	}
	events := d.journal.Since(0, 0)
	last := events[len(events)-1]
	if !strings.Contains(last.Title, "switched from justanotherspy/shuck#42") {
		t.Errorf("title = %q, want it to name both targets", last.Title)
	}
}

func TestDaemonDetachedHeadIsReportedOnce(t *testing.T) {
	c := newFakeClient()
	d, _ := newTestDaemon(t, c)

	dir := t.TempDir()
	writeRepo(t, dir, "0123456789abcdef0123456789abcdef01234567\n", "", "", originConfig)
	d.watches.Add(Watch{ID: TreeWatchID(dir), Kind: WatchTree, Path: dir})

	d.retarget(context.Background(), now)
	d.retarget(context.Background(), now.Add(time.Second))

	events := d.journal.Since(0, 0)
	if len(events) != 1 {
		t.Fatalf("%d events for one detached HEAD, want 1", len(events))
	}
	if !strings.Contains(events[0].Title, "detached") {
		t.Errorf("title = %q, want it to explain the detached HEAD", events[0].Title)
	}
	if c.calls("FindOpenPR") != 0 {
		t.Error("a detached HEAD has no branch to match, so it must not cost a lookup")
	}
}

func TestDaemonUnreadableTreeIsReported(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.watches.Add(Watch{ID: "tree:/nope", Kind: WatchTree, Path: "/nonexistent-directory"})

	d.retarget(context.Background(), now)

	events := d.journal.Since(0, 0)
	if len(events) != 1 || !strings.Contains(events[0].Title, "not inside a git repository") {
		t.Fatalf("expected the unreadable tree to be explained, got %+v", events)
	}
}

// TestDaemonDeduplicatesTargets is why watches and targets are separate: two
// people watching the same PR must cost one poll, not two.
func TestDaemonDeduplicatesTargets(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-1"
	d, _ := newTestDaemon(t, c)

	d.watches.Add(Watch{ID: "tree:/a", Kind: WatchTree, Path: "/a", Owner: "o", Repo: "r", Number: 7})
	d.watches.Add(Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7})

	due := d.due(now)
	if len(due) != 1 {
		t.Fatalf("%d targets due, want 1 shared between the two watches", len(due))
	}
	if due[0].Target != "o/r#7" {
		t.Errorf("target = %q, want o/r#7", due[0].Target)
	}
}

func TestDaemonDueRespectsTheDeadline(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.watches.Add(Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7})

	if got := d.due(now); len(got) != 1 {
		t.Fatalf("a new target should be due immediately, got %d", len(got))
	}
	// due() claims the slot, so a second tick a moment later finds nothing —
	// a slow poll must not queue up behind itself.
	if got := d.due(now.Add(time.Second)); len(got) != 0 {
		t.Errorf("%d targets due again a second later, want 0", len(got))
	}
	if got := d.due(now.Add(ActiveInterval + time.Second)); len(got) != 1 {
		t.Errorf("%d targets due after the interval, want 1", len(got))
	}
}

func TestDaemonPrunesTargetsOfRemovedWatches(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc")
	c.fingerprint = "fp"
	d, _ := newTestDaemon(t, c)

	d.watches.Add(Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7})
	d.due(now)
	if len(d.targets) != 1 {
		t.Fatalf("%d targets, want 1", len(d.targets))
	}

	d.watches.Remove("pr:o/r#7")

	// The first prune only marks it: a watch can lose its PR because a lookup
	// failed, and dropping the state then would replay everything it had
	// already reported.
	d.pruneTargets(now)
	if len(d.targets) != 1 {
		t.Fatalf("%d targets immediately after removal, want the state kept through the grace period", len(d.targets))
	}
	d.pruneTargets(now.Add(TargetGrace + time.Minute))
	if len(d.targets) != 0 {
		t.Errorf("%d targets after the grace period, want 0 — a session that moves through ten branches must not leave ten pollers behind", len(d.targets))
	}
}

// TestDaemonKeepsStateThroughATransientLookupFailure is the bug that grace
// period exists for: one failed FindOpenPR must not make the monitor forget
// which failures it had already reported.
func TestDaemonKeepsStateThroughATransientLookupFailure(t *testing.T) {
	c := newFakeClient()
	c.openPR = 42
	c.pr = openPR("abc1234def")
	c.pr.Number = 42
	c.fingerprint = "fp-1"
	d, _ := newTestDaemon(t, c)

	dir := treeAt(t, "feature")
	d.watches.Add(Watch{ID: TreeWatchID(dir), Kind: WatchTree, Path: dir})
	d.retarget(context.Background(), now)

	// Give the target some remembered state.
	for _, st := range d.due(now) {
		st.ReportedJobs = []string{"11/1"}
		d.store(st, nil)
	}
	target := "justanotherspy/shuck#42"
	if _, ok := d.targets[target]; !ok {
		t.Fatalf("no state for %s; have %v", target, d.targets)
	}

	// The next resolution fails transiently.
	c.openPRErr = errors.New("502 bad gateway")
	c.openPR = 0
	d.retarget(context.Background(), now.Add(ResolveInterval+time.Second))

	st, ok := d.targets[target]
	if !ok {
		t.Fatal("a single failed lookup threw away the target's poll state")
	}
	if len(st.ReportedJobs) != 1 {
		t.Errorf("ReportedJobs = %v, want what had already been reported to survive", st.ReportedJobs)
	}
}

func TestDaemonStoreAndPersistence(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-1"
	dir := t.TempDir()

	original := newPRClient
	newPRClient = func(string) prClient { return c }
	t.Cleanup(func() { newPRClient = original })

	d, err := newDaemon(dir, Options{Version: "test", NoPins: true})
	if err != nil {
		t.Fatal(err)
	}
	d.watches.Add(Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7})
	for _, st := range d.due(now) {
		updated, events := d.poller.Poll(context.Background(), st, now)
		d.store(updated, events)
	}

	// A restart must not replay a PR's history as if it had just happened.
	restarted, err := newDaemon(dir, Options{Version: "test", NoPins: true})
	if err != nil {
		t.Fatal(err)
	}
	st, ok := restarted.targets["o/r#7"]
	if !ok {
		t.Fatal("the target's poll state did not survive the restart")
	}
	if st.HeadSHA != "abc1234def" || st.ReviewFingerprint != "fp-1" {
		t.Errorf("restored state = %+v, want the head SHA and fingerprint preserved", st)
	}
}

func TestDaemonExpiresWatchesAndStops(t *testing.T) {
	c := newFakeClient()
	d, _ := newTestDaemon(t, c)
	d.opts.WatchTTL = time.Hour
	d.opts.ExitWhenIdle = true

	d.watches.Add(Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7})
	w, _ := d.watches.Get("pr:o/r#7")
	w.LastSeen = time.Now().Add(-2 * time.Hour)

	// A round with nothing left to watch tells the caller to shut down: a
	// laptop whose sessions have all ended should stop polling GitHub.
	if done := d.round(context.Background(), time.Now()); !done {
		t.Error("round should report the daemon is finished once its last watch expires")
	}
	if d.watches.Len() != 0 {
		t.Errorf("%d watches survived expiry, want 0", d.watches.Len())
	}
}

func TestDaemonRoundKeepsRunningWhenNotIdleExiting(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	if done := d.round(context.Background(), time.Now()); done {
		t.Error("a daemon started by hand should keep running with nothing to watch")
	}
}

func TestDaemonShutdownIsIdempotent(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.Shutdown()
	d.Shutdown() // a client and a signal may race; the second must not panic
	select {
	case <-d.stop:
	default:
		t.Error("Shutdown should close the stop channel")
	}
}

func TestDaemonNotifyWakesWaiters(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	wake := d.waiter()

	go d.publish([]Event{{Kind: KindCIFailed, Title: "red"}})

	select {
	case <-wake:
	case <-time.After(2 * time.Second):
		t.Fatal("a published event should wake a waiter")
	}
}

func TestDaemonPublishIgnoresEmptyBatches(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.publish(nil)
	if d.journal.Latest() != 0 {
		t.Error("an empty batch should not touch the journal")
	}
}

func TestDaemonRoundPollsAndJournals(t *testing.T) {
	c := newFakeClient()
	c.pr = openPR("abc1234def")
	c.fingerprint = "fp-1"
	c.failed = []model.JobResult{failedJob(11, "test")}
	c.jobLog = failingLog
	d, _ := newTestDaemon(t, c)

	d.watches.Add(Watch{ID: "pr:o/r#7", Kind: WatchPR, Owner: "o", Repo: "r", Number: 7})
	d.round(context.Background(), now)

	events := d.journal.Since(0, 0)
	if hasKind(events, KindCIFailed) == nil {
		t.Fatalf("the round should have journalled the failure, got %v", kinds(events))
	}
}
