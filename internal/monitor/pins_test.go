package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	st, events := d.scanPins(context.Background(), pinState{Path: dir}, now)

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
	st, events := d.scanPins(context.Background(), pinState{Path: dir}, now)
	if len(events) != 1 {
		t.Fatalf("%d events on the first audit, want 1", len(events))
	}

	// Past the interval, with the files unchanged: the audit runs again (a
	// release may have landed) but says nothing new.
	_, events = d.scanPins(context.Background(), st, now.Add(pinScanInterval+time.Minute))
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
	first, _ := d.scanPins(context.Background(), pinState{Path: dir}, now)
	calls := resolver.calls

	st := first
	for i := range 10 {
		st, _ = d.scanPins(context.Background(), st, now.Add(time.Duration(i+1)*time.Second))
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
	st, _ := d.scanPins(context.Background(), pinState{Path: dir}, now)
	if !st.NextScan.After(now) {
		t.Fatalf("NextScan = %v, want the tree paced past %v", st.NextScan, now)
	}

	write(t, filepath.Join(dir, ".github", "workflows", "ci.yml"),
		unpinnedWorkflow+"      - uses: actions/setup-go@v5\n")
	for i := range 10 {
		var events []Event
		st, events = d.scanPins(context.Background(), st, now.Add(time.Duration(i+1)*time.Second))
		if len(events) != 0 {
			t.Fatalf("tick %d re-read the tree and reported %v", i+1, kinds(events))
		}
	}
}

func TestScanPinsRunsWhenAFileChanges(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	d.opts.PinResolver = &stubResolver{tag: "v5.0.0", sha: "def"}

	dir := workflowTree(t, unpinnedWorkflow)
	st, _ := d.scanPins(context.Background(), pinState{Path: dir}, now)

	// Editing the workflow — the exact moment a pin audit is worth running,
	// and it runs on the next scan rather than on the keystroke.
	write(t, filepath.Join(dir, ".github", "workflows", "ci.yml"),
		unpinnedWorkflow+"      - uses: actions/setup-go@v5\n")

	_, events := d.scanPins(context.Background(), st, now.Add(pinScanInterval+time.Second))
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
	if _, events := d.scanPins(context.Background(), pinState{Path: dir}, now); len(events) != 0 {
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
	_, events := d.scanPins(context.Background(), pinState{Path: dir}, now)
	if len(events) != 1 {
		t.Fatalf("%d events, want 1 for the superseded pin", len(events))
	}
	if !strings.Contains(events[0].Title, "v4.9.9 is newer") {
		t.Errorf("title = %q, want it to name the newer release", events[0].Title)
	}
}

func TestScanPinsWithNoWorkflows(t *testing.T) {
	d, _ := newTestDaemon(t, newFakeClient())
	st, events := d.scanPins(context.Background(), pinState{Path: t.TempDir()}, now)
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
	_, events := d.scanPins(context.Background(), pinState{Path: dir}, now)

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

	restarted, err := newDaemon(d.paths.dir, Options{NoPins: false, Version: "test"})
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
