package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A Claude Code plugin monitor is the second way shuck's events reach a
// session: the plugin declares `shuck monitor stream` as a long-running
// command, and every line it writes arrives as a notification. That is a second
// consumer of the same journal, and the one thing two channels must not do is
// deliver the same thing twice — an agent handed a CI failure it has already
// acted on is worse off than one told about it late. So a running stream leaves
// a marker behind naming the working tree it serves, and the UserPromptSubmit
// hook stands down while one is live.
//
// The marker is written to fail toward delivery. A stream that is killed cannot
// remove its own file, and a hook that took a leftover marker for a live stream
// would silence the only channel still working — a monitor that has quietly
// stopped reporting, which is the worst state this package can be in. Liveness
// is therefore two independent claims, a fresh heartbeat and a process that
// still exists, and anything short of both counts as gone.

// streamStaleAfter is how long a marker outlives its last heartbeat. A stream
// spends its life blocked in one long read and refreshes the marker each time
// that read returns — at most a minute apart — so the window is generous on
// purpose: a marker that expired at the first missed beat would flap every time
// the feed went quiet, and each flap is a duplicated delivery.
const streamStaleAfter = 3 * time.Minute

// StreamConsumer is the journal identity a stream over watchID reads under. The
// prefix is not decoration: cursors are keyed by a bare string, Claude Code
// identifies a session by its own id, and a stream that collided with one would
// consume the very events the Stop hook exists to gate on.
func StreamConsumer(watchID string) string { return "stream:" + watchID }

// StreamIdentity is the identity a stream about to serve w should read under:
// the stable one, unless another stream is already live on the same tree.
//
// Stability is the point of the derived id — it is what lets a restarted stream
// resume rather than replay the journal at the session — but two sessions opened
// in one directory (two terminals, the ordinary way to run a second agent on a
// repo) would then share a cursor, and `Drain` is at-most-once per consumer: each
// CI failure would reach exactly one of the two. The second stream therefore
// takes an identity of its own, and starts from the present rather than from the
// backlog its sibling has already been delivering.
//
// Call it before BeginStream: afterwards this process's own marker is on disk
// and every stream would read as the second one.
func StreamIdentity(w Watch) string {
	id := StreamConsumer(w.ID)
	if w.Path != "" && StreamServes(w.Path) {
		return fmt.Sprintf("%s#%d", id, os.Getpid())
	}
	return id
}

// Stream is a running `shuck monitor stream`'s claim on a working tree.
type Stream struct {
	path string
	rec  streamRecord
}

// streamRecord is the marker's on-disk shape. Path is what the hooks match on
// rather than the watch id: the id is derived from the directory the stream was
// started in, and a session whose cwd is a subdirectory of that tree — or a
// symlink to it — would never match the id while plainly being the same work.
type streamRecord struct {
	Watch     string    `json:"watch"`
	Path      string    `json:"path,omitempty"`
	Consumer  string    `json:"consumer"`
	PID       int       `json:"pid"`
	Heartbeat time.Time `json:"heartbeat"`
}

// BeginStream records that this process is streaming w's events, replacing
// whatever an earlier run of this process left behind. It fails only when the
// marker cannot be written at all, which the caller must treat as a reason not
// to stream: without the marker the hooks keep delivering, and the session gets
// everything twice.
//
// The marker is one file per process, not per watch. Two streams can be live on
// one tree, and a shared file would let either of them revoke the other's claim
// by exiting — the surviving session's prompt hook would then start delivering
// again and hand it, as injected context, the events its stream has already
// shown it as notifications.
func BeginStream(w Watch, consumer string) (*Stream, error) {
	dir, err := streamsDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("create stream directory: %w", err)
	}
	sweepStreams(dir)

	s := &Stream{
		path: filepath.Join(dir, streamFile(w.ID, os.Getpid())),
		rec: streamRecord{
			Watch:     w.ID,
			Path:      w.Path,
			Consumer:  consumer,
			PID:       os.Getpid(),
			Heartbeat: time.Now(),
		},
	}
	if err := writeJSONFile(s.path, s.rec); err != nil {
		return nil, fmt.Errorf("record the stream marker: %w", err)
	}
	return s, nil
}

// Beat refreshes the heartbeat. A failure is deliberately silent: the marker
// simply ages out, the hooks take delivery back, and the session hears about
// its build from the other channel rather than not at all.
func (s *Stream) Beat() {
	if s == nil {
		return
	}
	s.rec.Heartbeat = time.Now()
	_ = writeJSONFile(s.path, s.rec)
}

// Close retires this process's marker, and only ever its own — a sibling stream
// on the same tree keeps its claim. It is safe to call more than once, which is
// what lets a stream drop its claim the moment it is interrupted and still clean
// up on the way out.
func (s *Stream) Close() {
	if s == nil {
		return
	}
	_ = os.Remove(s.path)
}

// StreamServes reports whether a live stream is already delivering the
// monitor's events for the working tree at dir.
//
// Matching is by containment in either direction, after resolving symlinks:
// a session started in a subdirectory of the tree the stream registered, or in
// the tree a stream registered a subdirectory of, is the same work either way.
// The asymmetry of the two mistakes is what settles it — matching too widely
// costs a delivery that waits for the next prompt, since standing down consumes
// nothing, while matching too narrowly hands the agent the same failure twice.
func StreamServes(dir string) bool {
	if dir == "" {
		return false
	}
	sdir, err := streamsDir()
	if err != nil {
		return false
	}
	entries, err := os.ReadDir(sdir)
	if err != nil {
		return false
	}
	now := time.Now()
	for _, entry := range entries {
		var rec streamRecord
		if readJSONFile(filepath.Join(sdir, entry.Name()), &rec) != nil {
			continue
		}
		if rec.live(now) && sameTree(rec.Path, dir) {
			return true
		}
	}
	return false
}

// live reports whether a marker still stands for a process that is streaming.
func (r streamRecord) live(now time.Time) bool {
	return now.Sub(r.Heartbeat) <= streamStaleAfter && processAlive(r.PID)
}

// sweepStreams drops the markers of streams that are gone. It runs when a
// stream starts rather than when a hook reads, because a hook must not write:
// leaving the files would cost nothing but tidiness, and a session is not the
// place to spend even that.
func sweepStreams(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		var rec streamRecord
		if readJSONFile(path, &rec) == nil && rec.live(now) {
			continue
		}
		_ = os.Remove(path)
	}
}

// streamsDir names the directory the markers live in. It does not create it —
// the read side must be able to answer "no stream" for a machine that has never
// run one without leaving a directory behind to prove it asked.
func streamsDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "streams"), nil
}

// streamFile names one stream's marker. The watch id is a path with a prefix, so
// it cannot be a filename; a hash of it can, and the id is stored inside the
// file for anyone reading the directory by hand. The pid completes the name so
// that two streams on one tree own two files: the readers all scan the directory
// and match on the recorded path, so nothing looks a marker up by name, and
// nobody can delete a claim that is not theirs.
func streamFile(watchID string, pid int) string {
	sum := sha256.Sum256([]byte(watchID))
	return fmt.Sprintf("%s-%d.json", hex.EncodeToString(sum[:8]), pid)
}

// sameTree reports whether two working-tree paths name the same piece of work.
func sameTree(a, b string) bool {
	ra, rb := resolveTree(a), resolveTree(b)
	if ra == "" || rb == "" {
		return false
	}
	return ra == rb || within(ra, rb) || within(rb, ra)
}

// resolveTree canonicalizes a path as far as the filesystem allows. A path that
// no longer exists cannot be resolved through its symlinks, so it degrades to
// the cleaned absolute form rather than to nothing.
func resolveTree(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// within reports whether child sits under parent.
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
