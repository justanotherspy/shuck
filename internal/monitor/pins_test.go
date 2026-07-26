package monitor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/action"
)

// stubResolver answers pin lookups from a fixed table.
type stubResolver struct {
	tag, sha string
	err      error
	calls    int
}

func (s *stubResolver) Resolve(_ context.Context, ref action.Ref) (action.Resolved, error) {
	s.calls++
	if s.err != nil {
		return action.Resolved{}, s.err
	}
	return action.Resolved{Ref: ref, Tag: s.tag, SHA: s.sha}, nil
}

// testPinRepo is the repository the single-tree scans are deduplicated under.
// Findings are keyed by repository, so a test that says nothing about which one
// still has to name it.
const testPinRepo = "justanotherspy/shuck"

// workflowTree writes a repository whose .github/workflows holds one file.
func workflowTree(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wf, "ci.yml"), content)
	return dir
}

const unpinnedWorkflow = `name: CI
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: make ci
`

func TestScanPinsReportsUnpinnedActions(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.PinResolver = &stubResolver{tag: "v4.2.2", sha: "3d3c42e5aac5ba805825da76410c181273ba90b1"}

	dir := workflowTree(t, unpinnedWorkflow)
	st, _, events := d.scanPins(context.Background(), pinState{Path: dir}, testPinRepo, maxRemembered, nil, now)

	if len(events) != 1 {
		t.Fatalf("%d events, want 1 for the unpinned action", len(events))
	}
	e := events[0]
	if e.Kind != KindPinsStale {
		t.Errorf("Kind = %q, want %q", e.Kind, KindPinsStale)
	}
	if !strings.Contains(e.Title, "actions/checkout@v4") || !strings.Contains(e.Title, "not SHA-pinned") {
		t.Errorf("title = %q, want it to name the reference and the problem", e.Title)
	}
	// The fix is the whole value of the finding, so the fix is the body.
	if !strings.Contains(e.Body, "3d3c42e5aac5ba805825da76410c181273ba90b1") || !strings.Contains(e.Body, "v4.2.2") {
		t.Errorf("body should carry the line to paste:\n%s", e.Body)
	}
	if !st.NextScan.After(now) {
		t.Errorf("state = %+v, want the next scan paced past %v", st, now)
	}
}

func TestScanPinsReportsEachFindingOnce(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.PinResolver = &stubResolver{tag: "v4.2.2", sha: "abc"}

	dir := workflowTree(t, unpinnedWorkflow)
	st, reported, events := d.scanPins(context.Background(), pinState{Path: dir}, testPinRepo, maxRemembered, nil, now)
	if len(events) != 1 {
		t.Fatalf("%d events on the first audit, want 1", len(events))
	}

	// Past the interval, with the files unchanged: the audit runs again (a
	// release may have landed) but says nothing new.
	_, _, events = d.scanPins(context.Background(), st, testPinRepo, maxRemembered, reported, now.Add(pinScanInterval+time.Minute))
	if len(events) != 0 {
		t.Errorf("an unchanged finding was reported twice: %v", kinds(events))
	}
}

// TestScanPinsBudgetsItsNetworkWork is one half of the pin budget: the daemon
// calls this for every watched tree on every one-second tick, and resolving an
// action's latest release is an API call. It also pins the contract auditPins
// leans on to decide whether to persist — a scan that was not due hands back
// the state it was given, deadline included.
func TestScanPinsBudgetsItsNetworkWork(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	resolver := &stubResolver{tag: "v4.2.2", sha: "abc"}
	d.opts.PinResolver = resolver

	dir := workflowTree(t, unpinnedWorkflow)
	first, _, _ := d.scanPins(context.Background(), pinState{Path: dir}, testPinRepo, maxRemembered, nil, now)
	calls := resolver.calls

	st := first
	for i := range 10 {
		st, _, _ = d.scanPins(context.Background(), st, testPinRepo, maxRemembered, nil, now.Add(time.Duration(i+1)*time.Second))
		if !st.same(first) {
			t.Fatalf("tick %d returned %+v, want the state untouched — auditPins persists anything else", i+1, st)
		}
	}
	if resolver.calls != calls {
		t.Errorf("resolver called %d more times over ten ticks inside the interval, want 0", resolver.calls-calls)
	}
}

// TestScanPinsPacesItsFilesystemWork is the other half of that budget, and the
// half a content fingerprint cannot cover: telling changed files from unchanged
// ones means reading and hashing all of them first. The daemon ticks once a
// second, so a scan inside the interval must not touch the tree at all — which
// is observable as an edit staying invisible until the interval is up.
func TestScanPinsPacesItsFilesystemWork(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.PinResolver = &stubResolver{tag: "v5.0.0", sha: "def"}

	dir := workflowTree(t, unpinnedWorkflow)
	st, _, _ := d.scanPins(context.Background(), pinState{Path: dir}, testPinRepo, maxRemembered, nil, now)
	if !st.NextScan.After(now) {
		t.Fatalf("NextScan = %v, want the tree paced past %v", st.NextScan, now)
	}

	write(t, filepath.Join(dir, ".github", "workflows", "ci.yml"),
		unpinnedWorkflow+"      - uses: actions/setup-go@v5\n")
	for i := range 10 {
		var events []Event
		st, _, events = d.scanPins(context.Background(), st, testPinRepo, maxRemembered, nil, now.Add(time.Duration(i+1)*time.Second))
		if len(events) != 0 {
			t.Fatalf("tick %d re-read the tree and reported %v", i+1, kinds(events))
		}
	}
}

func TestScanPinsRunsWhenAFileChanges(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.PinResolver = &stubResolver{tag: "v5.0.0", sha: "def"}

	dir := workflowTree(t, unpinnedWorkflow)
	st, reported, _ := d.scanPins(context.Background(), pinState{Path: dir}, testPinRepo, maxRemembered, nil, now)

	// Editing the workflow — the exact moment a pin audit is worth running,
	// and it runs on the next scan rather than on the keystroke.
	write(t, filepath.Join(dir, ".github", "workflows", "ci.yml"),
		unpinnedWorkflow+"      - uses: actions/setup-go@v5\n")

	_, _, events := d.scanPins(context.Background(), st, testPinRepo, maxRemembered, reported, now.Add(pinScanInterval+time.Second))
	if len(events) != 1 {
		t.Fatalf("%d events after an edit, want 1 for the newly added reference", len(events))
	}
	if !strings.Contains(events[0].Title, "actions/setup-go@v5") {
		t.Errorf("title = %q, want the new reference", events[0].Title)
	}
}

func TestScanPinsQuietOnACleanTree(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.PinResolver = &stubResolver{tag: "v4.2.2", sha: "3d3c42e5aac5ba805825da76410c181273ba90b1"}

	dir := workflowTree(t, `name: CI
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v4.2.2
`)
	if _, _, events := d.scanPins(context.Background(), pinState{Path: dir}, testPinRepo, maxRemembered, nil, now); len(events) != 0 {
		t.Errorf("a fully pinned, current workflow produced %v, want nothing", kinds(events))
	}
}

func TestScanPinsReportsStalePins(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.PinResolver = &stubResolver{tag: "v4.9.9", sha: "newsha"}

	dir := workflowTree(t, `name: CI
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v4.2.2
`)
	_, _, events := d.scanPins(context.Background(), pinState{Path: dir}, testPinRepo, maxRemembered, nil, now)
	if len(events) != 1 {
		t.Fatalf("%d events, want 1 for the superseded pin", len(events))
	}
	if !strings.Contains(events[0].Title, "v4.9.9 is newer") {
		t.Errorf("title = %q, want it to name the newer release", events[0].Title)
	}
}

func TestScanPinsWithNoWorkflows(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	st, _, events := d.scanPins(context.Background(), pinState{Path: t.TempDir()}, testPinRepo, maxRemembered, nil, now)
	if len(events) != 0 {
		t.Errorf("a repository with no workflows produced %v", kinds(events))
	}
	// Nothing to audit is exactly the tree that must not be walked every tick
	// for the rest of the session, so it is paced like any other.
	if !st.NextScan.After(now) {
		t.Errorf("NextScan = %v, want a tree with no workflows paced too", st.NextScan)
	}
}

func TestScanPinsSurvivesAResolverError(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.PinResolver = &stubResolver{err: errors.New("403 rate limited")}

	dir := workflowTree(t, unpinnedWorkflow)
	_, _, events := d.scanPins(context.Background(), pinState{Path: dir}, testPinRepo, maxRemembered, nil, now)

	// Whether a reference is pinned is a property of the reference, not of
	// whether the latest release could be looked up: a rate-limited audit must
	// still report the finding, just without a suggested fix.
	if len(events) != 1 {
		t.Fatalf("%d events with a failing resolver, want the unpinned reference reported anyway", len(events))
	}
	if strings.Contains(events[0].Body, "Replace the reference") {
		t.Errorf("no fix could be resolved, so none should be suggested:\n%s", events[0].Body)
	}
}

func TestDaemonAuditPinsAcrossWatchedTrees(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.NoPins = false
	d.opts.PinResolver = &stubResolver{tag: "v4.2.2", sha: "abc"}

	dir := workflowTree(t, unpinnedWorkflow)
	d.watches.Add(Watch{ID: TreeWatchID(dir), Kind: WatchTree, Path: dir})

	d.auditPins(context.Background(), now)

	if hasKind(d.journal.Since(0, 0), KindPinsStale) == nil {
		t.Fatal("a watched tree's unpinned action should be journalled")
	}
	// The state is persisted so a restart does not re-report it.
	var stored []pinState
	if err := readJSONFile(d.pinsPath(), &stored); err != nil {
		t.Fatalf("pin state was not persisted: %v", err)
	}
	if len(stored) != 1 || stored[0].Path != dir {
		t.Errorf("persisted %+v, want the audited tree", stored)
	}

	restarted, err := newDaemon(d.paths.dir, Options{NoPins: false, Version: "test", PinSubscribed: alwaysSubscribed})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := restarted.pins[dir]; !ok {
		t.Error("pin state did not survive the restart")
	}
}

// TestDaemonAuditPinsWritesOnlyWhenSomethingChanged guards the disk: auditPins
// runs on every one-second tick, and savePinsLocked marshals the whole pin map
// and rewrites pins.json. Doing that for a tick that learned nothing puts a
// write per watched tree per second behind a session that lasts hours.
func TestDaemonAuditPinsWritesOnlyWhenSomethingChanged(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.NoPins = false
	d.opts.PinResolver = &stubResolver{tag: "v4.2.2", sha: "abc"}

	dir := workflowTree(t, unpinnedWorkflow)
	d.watches.Add(Watch{ID: TreeWatchID(dir), Kind: WatchTree, Path: dir})

	d.auditPins(context.Background(), now)
	// Removing the file is what makes a later write observable: nothing but
	// savePinsLocked ever puts it back.
	if err := os.Remove(d.pinsPath()); err != nil {
		t.Fatalf("the first audit should have persisted the tree's state: %v", err)
	}

	for i := range 10 {
		d.auditPins(context.Background(), now.Add(time.Duration(i+1)*time.Second))
	}
	if _, err := os.Stat(d.pinsPath()); !os.IsNotExist(err) {
		t.Errorf("pins.json was rewritten by ticks with nothing to record (stat err = %v)", err)
	}

	// The other side of it: a scan that ran and found nothing new still moved
	// its deadline, and that has to reach disk — otherwise the deadline never
	// advances and the tree is re-read on every tick from then on.
	d.auditPins(context.Background(), now.Add(pinScanInterval+time.Minute))
	var stored []pinState
	if err := readJSONFile(d.pinsPath(), &stored); err != nil {
		t.Fatalf("a scan that moved the deadline was not persisted: %v", err)
	}
	if len(stored) != 1 || !stored[0].NextScan.After(now.Add(pinScanInterval)) {
		t.Errorf("persisted %+v, want the advanced scan deadline", stored)
	}
}

func TestDaemonAuditPinsRespectsNoPins(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient()) // NoPins is on by default here
	dir := workflowTree(t, unpinnedWorkflow)
	d.watches.Add(Watch{ID: TreeWatchID(dir), Kind: WatchTree, Path: dir})

	d.auditPins(context.Background(), now)

	if len(d.journal.Since(0, 0)) != 0 {
		t.Error("--no-pins should keep the monitor out of the .github directory entirely")
	}
}

// TestAuditPinsReportsARepositoryOnceAcrossWorktrees is the pin half of the
// spam this change exists to stop. The dedupe used to be keyed on the working
// tree, so N git worktrees of one checkout each reported the same stale action
// — N notifications about one line of one file. A finding is repo-relative, so
// the repository is what it belongs to.
func TestAuditPinsReportsARepositoryOnceAcrossWorktrees(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.NoPins = false
	d.opts.PinResolver = &stubResolver{tag: "v4.2.2", sha: "abc"}

	first := workflowTree(t, unpinnedWorkflow)
	second := workflowTree(t, unpinnedWorkflow)
	for _, dir := range []string{first, second} {
		d.watches.Add(Watch{
			ID: TreeWatchID(dir), Kind: WatchTree, Path: dir,
			Owner: "justanotherspy", Repo: "shuck",
		})
	}

	d.auditPins(context.Background(), now)
	if got := countKind(d.journal.Since(0, 0), KindPinsStale); got != 1 {
		t.Fatalf("%d pin events for two worktrees of one repository, want 1", got)
	}

	// And it survives the daemon: the reported set is persisted under the
	// repository, so a restart does not start the spam over.
	restarted, err := newDaemon(d.paths.dir, Options{
		NoPins:        false,
		Version:       "test",
		PinResolver:   &stubResolver{tag: "v4.2.2", sha: "abc"},
		PinSubscribed: alwaysSubscribed,
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted.auditPins(context.Background(), now.Add(pinScanInterval+time.Minute))
	if got := countKind(restarted.journal.Since(0, 0), KindPinsStale); got != 1 {
		t.Errorf("%d pin events after a restart, want the finding still reported once", got)
	}
}

// TestAuditPinsKeepsUnrelatedRepositoriesApart is the other direction: keying
// on the repository must not merge two checkouts that only look alike. Two
// repositories with the same unpinned action in the same file are two findings.
func TestAuditPinsKeepsUnrelatedRepositoriesApart(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.NoPins = false
	d.opts.PinResolver = &stubResolver{tag: "v4.2.2", sha: "abc"}

	one := workflowTree(t, unpinnedWorkflow)
	two := workflowTree(t, unpinnedWorkflow)
	d.watches.Add(Watch{ID: TreeWatchID(one), Kind: WatchTree, Path: one, Owner: "o", Repo: "one"})
	d.watches.Add(Watch{ID: TreeWatchID(two), Kind: WatchTree, Path: two, Owner: "o", Repo: "two"})

	d.auditPins(context.Background(), now)
	if got := countKind(d.journal.Since(0, 0), KindPinsStale); got != 2 {
		t.Errorf("%d pin events for two repositories, want one each", got)
	}
}

// TestPinRepoKeyFallsBackToTheTree covers the checkout with no GitHub remote to
// name. There is no repository identity to share, so it is deduplicated alone —
// exactly as every tree used to be.
func TestPinRepoKeyFallsBackToTheTree(t *testing.T) {
	tests := []struct {
		name string
		w    Watch
		want string
	}{
		{"a resolved checkout", Watch{Path: "/tree", Owner: "o", Repo: "r"}, "o/r"},
		{"no remote yet", Watch{Path: "/tree"}, "/tree"},
		{"half a remote is not one", Watch{Path: "/tree", Owner: "o"}, "/tree"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pinRepoKey(tt.w); got != tt.want {
				t.Errorf("pinRepoKey = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAuditPinsReachEveryWorktreeOfTheRepository is the other half of the
// per-repository dedupe, and the one that decides whether it is an improvement
// at all. Saying a finding once is only right if the one saying reaches every
// session that shares the repository: an event targeted at the tree that
// happened to scan first is filtered out of every other worktree's feed, and if
// that tree has no live session it reaches nobody, permanently.
func TestAuditPinsReachEveryWorktreeOfTheRepository(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.NoPins = false
	d.opts.PinResolver = &stubResolver{tag: "v4.2.2", sha: "abc"}

	first := workflowTree(t, unpinnedWorkflow)
	second := workflowTree(t, unpinnedWorkflow)
	for _, dir := range []string{first, second} {
		d.watches.Add(Watch{
			ID: TreeWatchID(dir), Kind: WatchTree, Path: dir,
			Owner: "o", Repo: "r", Number: 7, Scopes: []string{dir},
		})
	}

	d.auditPins(context.Background(), now)

	tests := []struct {
		name  string
		scope func() string
		want  int
	}{
		{"the worktree that scanned first", func() string { return first }, 1},
		{"and the one that had nothing new to say", func() string { return second }, 1},
		{"but not a session with no stake in the repository", func() string { return t.TempDir() }, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := d.drain(Request{Consumer: "sess:" + tt.name, Scope: tt.scope(), Peek: true})
			if got := countKind(events, KindPinsStale); got != tt.want {
				t.Errorf("%d pin events for %s, want %d", got, tt.name, tt.want)
			}
		})
	}
}

// TestAuditPinsKeysAWatchTheRoundHasNotResolvedYet covers the watch registered
// between a round's retarget and its pin audit: it still has no owner, so
// keying its first scan by path would file every finding under a name the next
// round abandons — reporting the lot again under the repository ten minutes
// later, and leaving a path-keyed entry behind that nothing ever prunes.
func TestAuditPinsKeysAWatchTheRoundHasNotResolvedYet(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.NoPins = false
	d.opts.PinResolver = &stubResolver{tag: "v4.2.2", sha: "abc"}

	tree := workflowTree(t, unpinnedWorkflow)
	writeRepo(t, tree, "ref: refs/heads/main\n", "refs/heads/main", "abc\n", originConfig)
	// As the registry holds it a tick after `shuck monitor watch`: a path, and
	// nothing resolved about it yet.
	d.watches.Add(Watch{ID: TreeWatchID(tree), Kind: WatchTree, Path: tree})

	d.auditPins(context.Background(), now)

	events := d.journal.Since(0, 0)
	if got := countKind(events, KindPinsStale); got != 1 {
		t.Fatalf("%d pin events, want 1", got)
	}
	if got := events[0].Target; got != testPinRepo {
		t.Errorf("Target = %q, want the repository %q — a path-keyed finding is one no sibling worktree hears", got, testPinRepo)
	}
	if _, ok := d.pinRepos[testPinRepo]; !ok {
		t.Errorf("pinrepos.json holds %v, want the findings filed under %q", slices.Sorted(maps.Keys(d.pinRepos)), testPinRepo)
	}
	if _, orphan := d.pinRepos[tree]; orphan {
		t.Errorf("the tree path was left behind as a key in %v", slices.Sorted(maps.Keys(d.pinRepos)))
	}

	// And the round that does resolve it says nothing a second time.
	w, _ := d.watches.Get(TreeWatchID(tree))
	w.Owner, w.Repo = "justanotherspy", "shuck"
	d.auditPins(context.Background(), now.Add(pinScanInterval+time.Minute))
	if got := countKind(d.journal.Since(0, 0), KindPinsStale); got != 1 {
		t.Errorf("%d pin events once the watch resolved, want the finding still reported once", got)
	}
}

// TestScanPinsDoesNotReReportAfterEviction is the spam the bound would
// otherwise cause. Nothing removes a key from a repository's reported set, so
// it accumulates one per line-shifting edit and per pin bump; once it passes
// maxRemembered a set trimmed in any time-free order evicts the same live
// findings on every scan and reports them again every pinScanInterval, forever.
func TestScanPinsDoesNotReReportAfterEviction(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.PinResolver = &stubResolver{tag: "v4.2.2", sha: "abc"}

	dir := workflowTree(t, unpinnedWorkflow)
	// A repository with a full set of history behind it: findings from files
	// and lines that no longer exist, every one of them sorting after the live
	// one so a text-ordered trim sacrifices exactly the wrong key.
	history := make([]string, 0, maxRemembered)
	for i := range maxRemembered {
		history = append(history, fmt.Sprintf("%s:.github/workflows/zz-old-%03d.yml:1:actions/checkout@v3", testPinRepo, i))
	}

	st, reported, events := d.scanPins(context.Background(), pinState{Path: dir}, testPinRepo, maxRemembered, history, now)
	if len(events) != 1 {
		t.Fatalf("%d events on the first audit, want 1", len(events))
	}
	if len(reported) > maxRemembered {
		t.Fatalf("the reported set grew to %d, want it bounded at %d", len(reported), maxRemembered)
	}

	for i := range 5 {
		at := now.Add(time.Duration(i+1) * (pinScanInterval + time.Minute))
		st, reported, events = d.scanPins(context.Background(), st, testPinRepo, maxRemembered, reported, at)
		if len(events) != 0 {
			t.Fatalf("scan %d reported %d findings again: %v", i+1, len(events), titles(events))
		}
		if len(reported) > maxRemembered {
			t.Fatalf("the reported set grew to %d on scan %d", len(reported), i+1)
		}
	}
}

// TestRememberPinsTrimsWhatNoRecentAuditHasSeen is the unit behind that: the
// keys this scan confirmed are the last things the bound may sacrifice.
func TestRememberPinsTrimsWhatNoRecentAuditHasSeen(t *testing.T) {
	history := make([]string, 0, maxRemembered)
	for i := range maxRemembered {
		history = append(history, fmt.Sprintf("aa-old-%03d", i))
	}

	tests := []struct {
		name     string
		previous []string
		current  []string
		want     []string // keys that must be in the result
		absent   []string
	}{
		{
			name:     "a confirmed finding survives a full set",
			previous: history,
			current:  []string{"zz-live"},
			want:     []string{"zz-live"},
			absent:   []string{"aa-old-000"},
		},
		{
			name:     "and so does one that sorts before everything",
			previous: history,
			current:  []string{"00-live"},
			want:     []string{"00-live"},
			absent:   []string{"aa-old-000"},
		},
		{
			name:     "a key the audit no longer sees keeps its place in front",
			previous: []string{"gone", "still-here"},
			current:  []string{"still-here"},
			want:     []string{"gone", "still-here"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rememberPins(tt.previous, tt.current, maxRemembered)
			if len(got) > maxRemembered {
				t.Fatalf("kept %d keys, want at most %d", len(got), maxRemembered)
			}
			for _, key := range tt.want {
				if !slices.Contains(got, key) {
					t.Errorf("%q was dropped; it would be reported again on the next scan", key)
				}
			}
			for _, key := range tt.absent {
				if slices.Contains(got, key) {
					t.Errorf("%q survived, but the bound had to give something up", key)
				}
			}
		})
	}
}

// TestAuditPinsReachTheTreeThatProducedThemBeforeItResolves is the delivery
// half of the mid-round registration. The finding is addressed to the
// repository, but the scope filter builds a session's accept set from the
// registry with the I/O-free pinRepoKey — so a watch the round has not resolved
// claims only its path, a sibling worktree that has resolved claims the
// repository, and the event is filtered out of the feed of the very session
// whose tree produced it. The cursor has already moved past it by then and the
// per-repository dedupe never says it again, so the loss is permanent.
func TestAuditPinsReachTheTreeThatProducedThemBeforeItResolves(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.NoPins = false
	d.opts.PinResolver = &stubResolver{tag: "v4.2.2", sha: "abc"}

	// The session that has been here a while: resolved, and with no workflow
	// files of its own, so every finding in this test comes from the other tree.
	settled := t.TempDir()
	d.watches.Add(Watch{
		ID: TreeWatchID(settled), Kind: WatchTree, Path: settled,
		Owner: "justanotherspy", Repo: "shuck", Number: 1, Scopes: []string{settled},
	})

	// And the one that registered between this round's retarget and its pin
	// audit: a path, a scope, and nothing resolved about it yet.
	fresh := workflowTree(t, unpinnedWorkflow)
	writeRepo(t, fresh, "ref: refs/heads/feature\n", "refs/heads/feature", "abc\n", originConfig)
	d.watches.Add(Watch{ID: TreeWatchID(fresh), Kind: WatchTree, Path: fresh, Scopes: []string{fresh}})

	d.auditPins(context.Background(), now)
	if got := countKind(d.journal.Since(0, 0), KindPinsStale); got != 1 {
		t.Fatalf("%d pin events, want 1 for the unpinned action", got)
	}

	events := d.drain(Request{Consumer: "sess:fresh", Scope: fresh})
	if got := countKind(events, KindPinsStale); got != 1 {
		t.Errorf("the session whose tree produced the finding received %d pin events, want 1", got)
	}
	if got := d.journal.Cursor("sess:fresh", 0); got == 0 {
		t.Fatal("the drain did not move the cursor; the redelivery risk this test is about is not present")
	}
	// And the sibling worktree still hears it: the repository key is shared on
	// purpose, and resolving one watch must not narrow the other's feed.
	if got := countKind(d.drain(Request{Consumer: "sess:settled", Scope: settled}), KindPinsStale); got != 1 {
		t.Errorf("the sibling worktree received %d pin events, want 1", got)
	}
}

// TestScanPinsKeepsWhatOneAuditConfirmed is the re-report loop the bound would
// cause on a repository with more live findings than it holds: the trim eats
// into the keys the scan just confirmed, so the next scan finds them unreported
// and says them again, every pinScanInterval, forever. Recency cannot
// discriminate — every key here was confirmed by the same audit.
func TestScanPinsKeepsWhatOneAuditConfirmed(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.PinResolver = &stubResolver{tag: "v4.2.2", sha: "abc"}

	const findings = maxRemembered + 25
	var wf strings.Builder
	wf.WriteString("name: CI\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n")
	for range findings {
		wf.WriteString("      - uses: actions/checkout@v4\n")
	}
	dir := workflowTree(t, wf.String())

	st, reported, events := d.scanPins(context.Background(), pinState{Path: dir}, testPinRepo, maxRemembered, nil, now)
	if len(events) != findings {
		t.Fatalf("%d events on the first audit, want %d", len(events), findings)
	}

	for i := range 3 {
		at := now.Add(time.Duration(i+1) * (pinScanInterval + time.Minute))
		st, reported, events = d.scanPins(context.Background(), st, testPinRepo, maxRemembered, reported, at)
		if len(events) != 0 {
			t.Fatalf("scan %d reported %d findings again with nothing changed: %v", i+1, len(events), titles(events)[:1])
		}
	}
}

// TestRememberPinsBudgetsEachWorktree is the same loop in the case the
// per-repository keying exists for. Two worktrees of one checkout share the
// reported set but sit on different branches, so their findings are different
// keys; a single flat bound between them means each tree's scan evicts the
// other's live findings and both re-report them on their next scan, forever.
func TestRememberPinsBudgetsEachWorktree(t *testing.T) {
	keys := func(prefix string, n int) []string {
		out := make([]string, 0, n)
		for i := range n {
			out = append(out, fmt.Sprintf("%s:.github/workflows/ci.yml:%d:actions/checkout@v4", prefix, i))
		}
		return out
	}
	const trees, each = 2, maxRemembered * 3 / 4
	budget := trees * maxRemembered

	first, second := keys("a", each), keys("b", each)
	got := rememberPins(nil, first, budget)
	got = rememberPins(got, second, budget)

	evicted := 0
	for _, key := range first {
		if !slices.Contains(got, key) {
			evicted++
		}
	}
	if evicted != 0 {
		t.Errorf("%d of the first worktree's live findings were evicted by the second worktree's scan, and will be reported again", evicted)
	}
	if len(got) > budget {
		t.Errorf("kept %d keys, want at most the %d the watched trees are budgeted", len(got), budget)
	}
}

// TestExpireReclaimsPinStateOfUnwatchedTrees covers the per-tree half of the
// pin state. A workflow that makes a git worktree per task registers a tree
// watch, gets a pinState keyed by its path, and then deletes the directory —
// and the entry used to outlive the watch, the tree and the daemon, being
// re-marshaled into pins.json every time any tree's deadline moved.
func TestExpireReclaimsPinStateOfUnwatchedTrees(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.NoPins = false
	d.opts.PinResolver = &stubResolver{tag: "v4.2.2", sha: "abc"}

	dir := workflowTree(t, unpinnedWorkflow)
	d.watches.Add(Watch{ID: TreeWatchID(dir), Kind: WatchTree, Path: dir, Owner: "o", Repo: "r"})
	d.auditPins(context.Background(), now)
	if _, ok := d.pins[dir]; !ok {
		t.Fatalf("the audited tree has no pin state: %v", slices.Sorted(maps.Keys(d.pins)))
	}

	// The session ends and nobody asks how things stand for a whole TTL.
	d.expire(time.Now().Add(2 * d.opts.WatchTTL))

	if _, ok := d.pins[dir]; ok {
		t.Errorf("the pin state of %q outlived its watch", dir)
	}
	var stored []pinState
	if err := readJSONFile(d.pinsPath(), &stored); err != nil || len(stored) != 0 {
		t.Errorf("pins.json holds %+v (err %v), want the dead tree gone from disk too", stored, err)
	}
	// The repository's reported set is deliberately longer-lived: dropping it
	// with the watch would re-announce every unfixed finding after each expiry.
	if _, ok := d.pinRepos["o/r"]; !ok {
		t.Errorf("the repository dedupe memory was reclaimed too: %v", slices.Sorted(maps.Keys(d.pinRepos)))
	}
}

// countKind counts the events of one kind, which is what the pin dedupe tests
// assert on: not that something was reported, but how many times.
func countKind(events []Event, kind Kind) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}
