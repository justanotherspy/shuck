package monitor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// streamHome points shuck's whole on-disk state at a temp directory, so a test
// never sees a stream the developer is actually running.
func streamHome(t *testing.T) {
	t.Helper()
	t.Setenv("SHUCK_HOME", t.TempDir())
}

// writeStreamMarker lays down a marker by hand, which is the only way to stage
// the states that matter: a stream that was killed, one whose heartbeat has
// aged out, one belonging to a process that no longer exists.
func writeStreamMarker(t *testing.T, rec streamRecord) string {
	t.Helper()
	dir, err := streamsDir()
	if err != nil {
		t.Fatalf("streamsDir: %v", err)
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, streamFile(rec.Watch, rec.PID))
	if err := writeJSONFile(path, rec); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	return path
}

// deadPID finds a process id nothing is using. A recycled pid would make the
// liveness test pass for the wrong reason, so it is searched for rather than
// assumed.
func deadPID(t *testing.T) int {
	t.Helper()
	for pid := 1 << 20; pid < 1<<22; pid++ {
		if !processAlive(pid) {
			return pid
		}
	}
	t.Skip("no unused pid to test a dead stream with")
	return 0
}

func TestStreamMarkerServesItsTree(t *testing.T) {
	streamHome(t)
	tree := t.TempDir()
	sub := filepath.Join(tree, "internal", "monitor")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	s, err := BeginStream(Watch{ID: TreeWatchID(tree), Kind: WatchTree, Path: tree}, StreamConsumer(TreeWatchID(tree)))
	if err != nil {
		t.Fatalf("BeginStream: %v", err)
	}

	tests := []struct {
		name string
		dir  string
		want bool
	}{
		{"the tree itself", tree, true},
		{"a subdirectory of it, which is the same work", sub, true},
		{"somewhere else entirely", t.TempDir(), false},
		{"no directory at all", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StreamServes(tt.dir); got != tt.want {
				t.Errorf("StreamServes(%q) = %v, want %v", tt.dir, got, tt.want)
			}
		})
	}

	// Closing hands delivery back: the hooks are the only channel again, and
	// they must not still believe otherwise.
	s.Close()
	if StreamServes(tree) {
		t.Error("a closed stream still claims its tree, so nothing will deliver its events")
	}
	// Twice is allowed — an interrupt drops the claim and the deferred cleanup
	// runs anyway.
	s.Close()
}

// TestStreamMarkerStaleness is the rule that keeps a dead stream from silencing
// the hooks. A marker outlives the process that wrote it, so believing one is
// how a session ends up hearing about a red build from nobody at all.
func TestStreamMarkerStaleness(t *testing.T) {
	tests := []struct {
		name string
		rec  func(t *testing.T, tree string) streamRecord
		want bool
	}{
		{
			name: "a fresh beat from a running process",
			rec: func(t *testing.T, tree string) streamRecord {
				return streamRecord{Watch: TreeWatchID(tree), Path: tree, PID: os.Getpid(), Heartbeat: time.Now()}
			},
			want: true,
		},
		{
			name: "a heartbeat that stopped being refreshed",
			rec: func(t *testing.T, tree string) streamRecord {
				return streamRecord{Watch: TreeWatchID(tree), Path: tree, PID: os.Getpid(),
					Heartbeat: time.Now().Add(-streamStaleAfter - time.Minute)}
			},
			want: false,
		},
		{
			name: "a process that is not there any more",
			rec: func(t *testing.T, tree string) streamRecord {
				return streamRecord{Watch: TreeWatchID(tree), Path: tree, PID: deadPID(t), Heartbeat: time.Now()}
			},
			// Only where a pid can be probed at all: elsewhere the heartbeat is
			// the whole test, and a fresh one reads as live on purpose.
			want: !canProbePIDs(),
		},
		{
			name: "a marker with no pid recorded",
			rec: func(t *testing.T, tree string) streamRecord {
				return streamRecord{Watch: TreeWatchID(tree), Path: tree, Heartbeat: time.Now()}
			},
			want: !canProbePIDs(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			streamHome(t)
			tree := t.TempDir()
			writeStreamMarker(t, tt.rec(t, tree))
			if got := StreamServes(tree); got != tt.want {
				t.Errorf("StreamServes = %v, want %v", got, tt.want)
			}
		})
	}
}

// canProbePIDs reports whether this platform can tell a live process from a
// dead one. Where it cannot, processAlive answers yes by design and the
// heartbeat is the only test — see stream_other.go.
func canProbePIDs() bool { return !processAlive(deadPIDProbe) }

// deadPIDProbe is a pid no system assigns. It is a constant rather than a
// search because canProbePIDs only has to answer "can this platform tell".
const deadPIDProbe = 1 << 30

func TestStreamServesNothingWhenNoStreamHasEverRun(t *testing.T) {
	streamHome(t)
	// The read side must answer this without creating anything: a machine that
	// has never streamed should not acquire a directory for having been asked.
	if StreamServes(t.TempDir()) {
		t.Error("a machine with no streams claims to have one")
	}
	dir, err := streamsDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("asking created %s; a hook must not write", dir)
	}
}

// TestStreamServesIgnoresRubbish covers the directory a crash or an unrelated
// tool can leave behind: nothing in it may make the hooks stand down.
func TestStreamServesIgnoresRubbish(t *testing.T) {
	streamHome(t)
	dir, err := streamsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-json.json"), []byte("{{{"), filePerm); err != nil {
		t.Fatal(err)
	}
	if StreamServes(t.TempDir()) {
		t.Error("an unparseable marker was taken for a live stream")
	}
}

// TestBeginStreamClearsWhatAKilledStreamLeft keeps the marker directory from
// growing a file per session that ever crashed. The sweep runs when a stream
// starts rather than when a hook reads, because a hook must not write.
func TestBeginStreamClearsWhatAKilledStreamLeft(t *testing.T) {
	streamHome(t)
	gone := t.TempDir()
	stale := writeStreamMarker(t, streamRecord{
		Watch: TreeWatchID(gone), Path: gone, PID: os.Getpid(),
		Heartbeat: time.Now().Add(-streamStaleAfter - time.Hour),
	})

	tree := t.TempDir()
	if _, err := BeginStream(Watch{ID: TreeWatchID(tree), Kind: WatchTree, Path: tree}, "stream:x"); err != nil {
		t.Fatalf("BeginStream: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the dead stream's marker survived: %v", err)
	}
	if !StreamServes(tree) {
		t.Error("the sweep took the new stream's own marker with it")
	}
}

// TestBeginStreamReplacesItsOwnMarker covers the restart: the same tree, a new
// process, and no second file claiming the first one is still running.
func TestBeginStreamReplacesItsOwnMarker(t *testing.T) {
	streamHome(t)
	tree := t.TempDir()
	w := Watch{ID: TreeWatchID(tree), Kind: WatchTree, Path: tree}

	first, err := BeginStream(w, "stream:one")
	if err != nil {
		t.Fatalf("BeginStream: %v", err)
	}
	second, err := BeginStream(w, "stream:two")
	if err != nil {
		t.Fatalf("BeginStream: %v", err)
	}
	if first.path != second.path {
		t.Errorf("one watch produced two markers: %s and %s", first.path, second.path)
	}

	var rec streamRecord
	if err := readJSONFile(second.path, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Consumer != "stream:two" {
		t.Errorf("consumer = %q, want the running stream's own", rec.Consumer)
	}

	// A beat keeps the claim current without changing what it claims.
	before := rec.Heartbeat
	time.Sleep(2 * time.Millisecond)
	second.Beat()
	if err := readJSONFile(second.path, &rec); err != nil {
		t.Fatal(err)
	}
	if !rec.Heartbeat.After(before) {
		t.Errorf("heartbeat = %s, want it moved forward from %s", rec.Heartbeat, before)
	}
	if rec.Watch != w.ID || rec.Consumer != "stream:two" {
		t.Errorf("a beat rewrote what the marker claims: %+v", rec)
	}
}

// TestStreamConsumerCannotCollideWithASession is why the id is prefixed at all.
// Cursors are keyed by a bare string, and a stream sharing a session's key would
// consume the events the Stop hook exists to block on.
func TestStreamConsumerCannotCollideWithASession(t *testing.T) {
	session := "9f8a2c1e-0b7d-4a55-9c3f-2e1d0a7b6c54"
	if got := StreamConsumer(session); got == session {
		t.Errorf("StreamConsumer(%q) = %q, want a namespace of its own", session, got)
	}
	if a, b := StreamConsumer("tree:/a"), StreamConsumer("tree:/b"); a == b {
		t.Errorf("two trees share one cursor: %q", a)
	}
	// The same tree twice is the same identity — that is what lets a restarted
	// stream resume instead of replaying the journal at the session.
	if a, b := StreamConsumer("tree:/a"), StreamConsumer("tree:/a"); a != b {
		t.Errorf("the identity is not stable: %q vs %q", a, b)
	}
}

// liveSiblingPID finds a pid that is alive and is not this process, which is the
// only way to stage the state that matters here: a second stream running in the
// same working tree.
func liveSiblingPID(t *testing.T) int {
	t.Helper()
	pid := os.Getppid()
	if pid <= 0 || pid == os.Getpid() || !processAlive(pid) {
		t.Skip("no second live process to stage a concurrent stream with")
	}
	return pid
}

// TestConcurrentStreamsOnOneTreeKeepSeparateClaims covers two Claude Code
// sessions opened in one checkout — two terminals, the ordinary way to run a
// second agent on a repo. Sharing one marker file meant the first to exit
// deleted the survivor's claim, and the survivor's prompt hook then re-injected
// as context the events its own stream had already delivered as notifications.
func TestConcurrentStreamsOnOneTreeKeepSeparateClaims(t *testing.T) {
	streamHome(t)
	tree := t.TempDir()
	w := Watch{ID: TreeWatchID(tree), Kind: WatchTree, Path: tree}

	// The sibling session, already streaming this tree.
	sibling := writeStreamMarker(t, streamRecord{
		Watch: w.ID, Path: tree, Consumer: StreamConsumer(w.ID),
		PID: liveSiblingPID(t), Heartbeat: time.Now(),
	})

	mine, err := BeginStream(w, StreamIdentity(w))
	if err != nil {
		t.Fatalf("BeginStream: %v", err)
	}
	if mine.path == sibling {
		t.Fatalf("both streams claim %s; either one's exit revokes the other's claim", mine.path)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("starting a second stream disturbed the first one's marker: %v", err)
	}

	// This session ends. The other is still streaming, so the hooks must stay
	// stood down for that tree.
	mine.Close()
	if _, err := os.Stat(mine.path); !os.IsNotExist(err) {
		t.Errorf("the closed stream's own marker survived: %v", err)
	}
	if !StreamServes(tree) {
		t.Error("one session's exit revoked the other's claim; the surviving session will be handed its notifications a second time")
	}
}

// TestStreamIdentitySeparatesConcurrentStreams is the cursor half of the same
// story: Drain is at-most-once per consumer, so two streams reading under one
// identity would split the feed — each CI failure notified to whichever of the
// two sessions happened to wake first.
func TestStreamIdentitySeparatesConcurrentStreams(t *testing.T) {
	streamHome(t)
	tree := t.TempDir()
	w := Watch{ID: TreeWatchID(tree), Kind: WatchTree, Path: tree}

	// Nobody else is streaming: the identity is the stable, derived one, which
	// is what lets a restarted stream resume instead of replaying the journal.
	if got := StreamIdentity(w); got != StreamConsumer(w.ID) {
		t.Errorf("StreamIdentity = %q, want the stable %q for the only stream on a tree", got, StreamConsumer(w.ID))
	}

	writeStreamMarker(t, streamRecord{
		Watch: w.ID, Path: tree, Consumer: StreamConsumer(w.ID),
		PID: liveSiblingPID(t), Heartbeat: time.Now(),
	})

	second := StreamIdentity(w)
	if second == StreamConsumer(w.ID) {
		t.Errorf("StreamIdentity = %q for a tree already being streamed; two sessions would share one cursor and split the feed", second)
	}
	if !strings.HasPrefix(second, "stream:") {
		t.Errorf("StreamIdentity = %q, want an identity that still cannot collide with a session's", second)
	}
	// A session started in a subdirectory of the streamed tree is the same work,
	// and gets its own identity too.
	sub := filepath.Join(tree, "internal")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	nested := Watch{ID: TreeWatchID(sub), Kind: WatchTree, Path: sub}
	if got := StreamIdentity(nested); got == StreamConsumer(nested.ID) {
		t.Errorf("StreamIdentity = %q for a subdirectory of a streamed tree, want one of its own", got)
	}
}

func TestSameTree(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "internal", "monitor")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical paths", root, root, true},
		{"a trailing separator is the same directory", root, root + string(filepath.Separator), true},
		{"a subdirectory is the same work", root, nested, true},
		{"and so is the other way round", nested, root, true},
		{"unrelated directories are not", root, t.TempDir(), false},
		{"an empty path names nothing", "", root, false},
		{"neither does two of them", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameTree(tt.a, tt.b); got != tt.want {
				t.Errorf("sameTree(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestSameTreeSeesThroughASymlink covers the case that would otherwise produce
// two watch ids for one checkout: the stream registers the real path and the
// session names the link, or the reverse.
func TestSameTreeSeesThroughASymlink(t *testing.T) {
	checkout := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(checkout, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	if !sameTree(checkout, link) {
		t.Errorf("sameTree(%q, %q) = false; a symlinked checkout would be delivered twice", checkout, link)
	}
}

func TestProcessAliveKnowsThisProcess(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("this process reports itself as gone")
	}
	for _, pid := range []int{0, -1} {
		if processAlive(pid) {
			t.Errorf("processAlive(%d) = true, want false — that is not a process id", pid)
		}
	}
}
