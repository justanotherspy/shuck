package monitor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testJournal(t *testing.T) *journal {
	t.Helper()
	j, err := openJournal(newPaths(t.TempDir()))
	if err != nil {
		t.Fatalf("openJournal: %v", err)
	}
	return j
}

func appendN(t *testing.T, j *journal, n int) {
	t.Helper()
	for i := range n {
		j.Append(Event{Kind: KindCIFailed, Title: "failure " + string(rune('a'+i%26))})
	}
}

func TestJournalAppendAssignsIDsAndTimes(t *testing.T) {
	j := testJournal(t)

	first := j.Append(Event{Kind: KindCIPassed, Title: "green"})
	second := j.Append(Event{Kind: KindCIFailed, Title: "red"})

	if first.ID != 1 || second.ID != 2 {
		t.Errorf("IDs = %d, %d, want 1, 2", first.ID, second.ID)
	}
	if first.Time.IsZero() {
		t.Error("an event with no time should be stamped on append")
	}
	if got := j.Latest(); got != 2 {
		t.Errorf("Latest = %d, want 2", got)
	}
}

// TestJournalDrainIsPerConsumer is the property the whole hook integration
// rests on: two sessions each see every event once, and neither consumes the
// other's backlog.
func TestJournalDrainIsPerConsumer(t *testing.T) {
	j := testJournal(t)
	j.Append(Event{Kind: KindCIFailed, Title: "one"})
	j.Append(Event{Kind: KindCIPassed, Title: "two"})

	a := j.Drain("session-a", 0)
	if len(a) != 2 {
		t.Fatalf("session-a drained %d events, want 2", len(a))
	}
	if again := j.Drain("session-a", 0); len(again) != 0 {
		t.Errorf("session-a drained %d events on the second call, want 0", len(again))
	}
	if b := j.Drain("session-b", 0); len(b) != 2 {
		t.Errorf("session-b drained %d events, want its own copy of both", len(b))
	}

	j.Append(Event{Kind: KindReviewComment, Title: "three"})
	if a := j.Drain("session-a", 0); len(a) != 1 || a[0].Title != "three" {
		t.Errorf("session-a should now see only the new event, got %d", len(a))
	}
}

func TestJournalAnonymousDrainConsumesNothing(t *testing.T) {
	j := testJournal(t)
	j.Append(Event{Kind: KindCIFailed, Title: "one"})

	if got := j.Drain("", 0); len(got) != 1 {
		t.Fatalf("anonymous drain returned %d, want 1", len(got))
	}
	if got := j.Drain("", 0); len(got) != 1 {
		t.Error("an anonymous drain must not advance any cursor")
	}
}

func TestJournalLimitKeepsTheNewest(t *testing.T) {
	j := testJournal(t)
	for _, title := range []string{"one", "two", "three"} {
		j.Append(Event{Kind: KindCIFailed, Title: title})
	}

	got := j.Drain("s", 2)
	if len(got) != 2 {
		t.Fatalf("drained %d, want 2", len(got))
	}
	// A consumer that has fallen behind wants the current state of the world.
	if got[0].Title != "two" || got[1].Title != "three" {
		t.Errorf("drained %q, %q — want the newest two", got[0].Title, got[1].Title)
	}
	if pending := j.Pending("s"); pending != 0 {
		t.Errorf("Pending = %d after a capped drain, want 0 (the cursor moved past the batch)", pending)
	}
}

func TestJournalSeekAndPending(t *testing.T) {
	j := testJournal(t)
	j.Append(Event{Kind: KindCIFailed})
	j.Append(Event{Kind: KindCIPassed})

	// A session starting now should hear what happens next, not the backlog.
	j.Seek("fresh", j.Latest())
	if got := j.Pending("fresh"); got != 0 {
		t.Errorf("Pending = %d after seeking to the present, want 0", got)
	}
	j.Append(Event{Kind: KindReviewComment})
	if got := j.Pending("fresh"); got != 1 {
		t.Errorf("Pending = %d, want 1", got)
	}
	// Seeking an anonymous consumer is a no-op rather than an error.
	j.Seek("", 1)
}

func TestJournalCursorHonoursOverride(t *testing.T) {
	j := testJournal(t)
	j.Append(Event{Kind: KindCIFailed})
	j.Seek("s", 1)

	if got := j.Cursor("s", 0); got != 1 {
		t.Errorf("Cursor = %d, want the stored cursor 1", got)
	}
	if got := j.Cursor("s", 5); got != 5 {
		t.Errorf("Cursor = %d, want the explicit override 5", got)
	}
}

// TestJournalSurvivesRestart is why the journal is on disk at all: a session
// that reconnects after the daemon was restarted must not be told everything
// is fine because the failure died with the previous process.
func TestJournalSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	p := newPaths(dir)

	first, err := openJournal(p)
	if err != nil {
		t.Fatal(err)
	}
	first.Append(Event{Kind: KindCIFailed, Title: "the build is red", Body: "line one\nline two"})
	first.Drain("session-a", 0) // session-a has seen everything so far
	first.Append(Event{Kind: KindReviewComment, Title: "alice commented"})

	second, err := openJournal(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Latest(); got != 2 {
		t.Errorf("Latest = %d after reopening, want 2", got)
	}
	pending := second.Drain("session-a", 0)
	if len(pending) != 1 || pending[0].Title != "alice commented" {
		t.Fatalf("after a restart session-a should still be owed exactly the second event, got %d", len(pending))
	}
	// A new event continues the sequence rather than colliding with a stored one.
	if e := second.Append(Event{Kind: KindCIPassed}); e.ID != 3 {
		t.Errorf("next ID = %d, want 3", e.ID)
	}
	// The body survived the round trip intact.
	all := second.Since(0, 0)
	if all[0].Body != "line one\nline two" {
		t.Errorf("body = %q, want it preserved across the restart", all[0].Body)
	}
}

func TestJournalSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	p := newPaths(dir)

	// A crash can clip the last line; losing one event must not stop the
	// monitor from starting.
	content := `{"id":1,"kind":"ci.failed","title":"good"}
not json at all
{"id":2,"kind":"ci.passed","tit
{"id":3,"kind":"ci.passed","title":"also good"}
`
	if err := os.WriteFile(p.journal, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	j, err := openJournal(p)
	if err != nil {
		t.Fatalf("openJournal: %v", err)
	}
	events := j.Since(0, 0)
	if len(events) != 2 {
		t.Fatalf("recovered %d events, want the 2 intact ones", len(events))
	}
	if j.Latest() != 3 {
		t.Errorf("Latest = %d, want 3 — IDs must not be reused after a gap", j.Latest())
	}
}

func TestJournalRotates(t *testing.T) {
	dir := t.TempDir()
	p := newPaths(dir)
	j, err := openJournal(p)
	if err != nil {
		t.Fatal(err)
	}

	appendN(t, j, maxJournalEvents+50)

	if got := len(j.Since(0, 0)); got > maxJournalEvents {
		t.Errorf("retained %d events, want the window capped at %d", got, maxJournalEvents)
	}
	// The file is rewritten, not just trimmed in memory.
	raw, err := os.ReadFile(p.journal)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(raw), "\n"); lines > maxJournalEvents {
		t.Errorf("journal file holds %d lines, want at most %d", lines, maxJournalEvents)
	}
	if j.Latest() != uint64(maxJournalEvents+50) {
		t.Errorf("Latest = %d, want IDs to keep climbing past rotation", j.Latest())
	}
}

func TestJournalPrunesStaleCursors(t *testing.T) {
	dir := t.TempDir()
	p := newPaths(dir)
	j, err := openJournal(p)
	if err != nil {
		t.Fatal(err)
	}

	j.Append(Event{Kind: KindCIFailed})
	j.Seek("ancient", 1)
	appendN(t, j, 3*maxJournalEvents)
	j.Seek("current", j.Latest())

	raw, err := os.ReadFile(p.cursors)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ancient") {
		t.Error("a cursor pointing far before the retained window should be forgotten, not kept forever")
	}
	if !strings.Contains(string(raw), "current") {
		t.Error("a live cursor must be persisted")
	}
}

func TestJournalIgnoresUnreadableCursors(t *testing.T) {
	dir := t.TempDir()
	p := newPaths(dir)
	if err := os.WriteFile(p.cursors, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := openJournal(p)
	if err != nil {
		t.Fatalf("a corrupt cursor file should not stop the monitor: %v", err)
	}
	if got := j.Pending("anyone"); got != 0 {
		t.Errorf("Pending = %d on a fresh journal, want 0", got)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "state.json")

	if err := writeFileAtomic(name, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(name, []byte("second")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("content = %q, want the second write", got)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	// The journal holds private-repo CI logs; the permissions are load bearing.
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("mode = %v, want %v", perm, filePerm)
	}
	// No temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("%d files in the directory, want just the target", len(entries))
	}

	if err := writeFileAtomic(filepath.Join(dir, "missing", "x"), []byte("x")); err == nil {
		t.Error("writing into a missing directory should fail")
	}
}

func TestReadWriteJSONFile(t *testing.T) {
	name := filepath.Join(t.TempDir(), "v.json")

	type record struct {
		Name string    `json:"name"`
		When time.Time `json:"when"`
	}
	want := record{Name: "x", When: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := writeJSONFile(name, want); err != nil {
		t.Fatal(err)
	}
	var got record
	if err := readJSONFile(name, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != want.Name || !got.When.Equal(want.When) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}

	if err := readJSONFile(filepath.Join(t.TempDir(), "nope.json"), &got); err == nil {
		t.Error("reading a missing file should report it")
	}
	if err := os.WriteFile(name, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := readJSONFile(name, &got); err == nil {
		t.Error("reading malformed JSON should report it")
	}
	if err := writeJSONFile(name, func() {}); err == nil {
		t.Error("writing an unencodable value should report it")
	}
}
