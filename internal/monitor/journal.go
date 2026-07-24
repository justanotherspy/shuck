package monitor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// maxJournalEvents bounds the journal, and rotateTo is what a rotation trims
// back to. The gap between them is deliberate: rotation rewrites the whole
// file, so trimming all the way down means one rewrite every few hundred
// events rather than one on every append past the cap.
//
// Two thousand events is far more than any session drains and small enough
// that a full read stays instant. The monitor is a live feed, not an archive:
// history older than that belongs in the pull request, not here.
const (
	maxJournalEvents = 2000
	rotateTo         = 1500
)

// journal is the daemon's durable event log: an append-only JSONL file plus the
// per-consumer cursors that make delivery exactly-once for each consumer.
//
// Durability matters because the daemon outlives the sessions reading from it
// and can be restarted underneath them. A session that reconnects after a
// daemon restart must not be told CI is fine because the failure event died
// with the previous process — so events and cursors both survive on disk, and a
// consumer's cursor is only advanced once its events have actually been handed
// over.
type journal struct {
	mu      sync.Mutex
	path    string
	cursors string

	next   uint64  // ID to assign to the next appended event
	events []Event // the retained window, oldest first
	marks  map[string]uint64
}

// openJournal loads the journal at p, recovering whatever survived a previous
// run. A corrupt or truncated line is skipped rather than treated as fatal:
// losing one event is a much better outcome than a monitor that refuses to
// start because its log got clipped by a crash.
func openJournal(p paths) (*journal, error) {
	j := &journal{path: p.journal, cursors: p.cursors, next: 1, marks: map[string]uint64{}}

	if err := j.loadEvents(); err != nil {
		return nil, err
	}
	if err := j.loadCursors(); err != nil {
		return nil, err
	}
	return j, nil
}

func (j *journal) loadEvents() error {
	f, err := os.Open(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open event journal: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// CI logs make for long lines; give the scanner room before it gives up.
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var last uint64
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		// IDs are cursors, so they must ascend: a consumer's stored position
		// only means anything if every event past it is newer. The log is
		// written that way, so an ID that does not advance is damage — a
		// half-written line that parsed, or a file two processes appended to —
		// and keeping it would make one consumer's cursor skip another's
		// events. Drop it rather than trust it.
		if e.ID <= last {
			continue
		}
		last = e.ID
		j.events = append(j.events, e)
		j.next = e.ID + 1
	}
	// A scanner error (an over-long line, a read fault) leaves the events read
	// so far intact, which is the useful outcome.
	return nil
}

func (j *journal) loadCursors() error {
	raw, err := os.ReadFile(j.cursors)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read consumer cursors: %w", err)
	}
	if err := json.Unmarshal(raw, &j.marks); err != nil || j.marks == nil {
		j.marks = map[string]uint64{}
	}
	return nil
}

// Append records an event, assigns its ID, and returns the stored copy.
func (j *journal) Append(e Event) Event {
	j.mu.Lock()
	defer j.mu.Unlock()

	e.ID = j.next
	j.next++
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	j.events = append(j.events, e)

	if len(j.events) > maxJournalEvents {
		j.events = j.events[len(j.events)-rotateTo:]
		j.rewriteLocked()
		return e
	}
	j.appendLocked(e)
	return e
}

// appendLocked writes one event to the end of the log. A write failure is not
// propagated: the event is already in memory and will be served to live
// consumers, and refusing to monitor because the disk is full would trade a
// small loss for a total one. The next rotation gets another chance.
func (j *journal) appendLocked(e Event) {
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// rewriteLocked replaces the log with the retained window, atomically.
func (j *journal) rewriteLocked() {
	var buf []byte
	for _, e := range j.events {
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		buf = append(append(buf, line...), '\n')
	}
	_ = writeFileAtomic(j.path, buf)
}

// Since returns every retained event with an ID greater than after, oldest
// first, capped at limit (0 means no cap).
func (j *journal) Since(after uint64, limit int) []Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.sinceLocked(after, limit)
}

func (j *journal) sinceLocked(after uint64, limit int) []Event {
	var out []Event
	for _, e := range j.events {
		if e.ID > after {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		// Keep the newest when the batch overflows: a consumer that has fallen
		// behind wants the current state of the world, not the start of a
		// backlog it will never catch up on.
		out = out[len(out)-limit:]
	}
	return out
}

// Drain returns the events a consumer has not seen and advances its cursor past
// them. A consumer is any stable string — a Claude Code session ID, say. The
// empty consumer is not tracked, so an anonymous caller can peek without
// consuming anyone's backlog.
//
// The cursor is advanced before the caller has done anything with the events,
// which makes delivery at-most-once for a given consumer. That is the right
// trade for this feed: repeating a CI failure into a session that already acted
// on it is worse than missing the tail of a batch nobody read.
func (j *journal) Drain(consumer string, limit int) []Event {
	j.mu.Lock()
	defer j.mu.Unlock()

	events := j.sinceLocked(j.marks[consumer], limit)
	if consumer == "" || len(events) == 0 {
		return events
	}
	j.marks[consumer] = events[len(events)-1].ID
	j.saveCursorsLocked()
	return events
}

// Latest reports the highest ID assigned so far, which is what a consumer
// starting fresh should treat as its cursor: a session that begins now cares
// about what happens next, not about the last hour of another session's CI.
func (j *journal) Latest() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.next - 1
}

// Seek moves a consumer's cursor without delivering anything, so a session can
// declare "I have seen everything up to here" as it starts.
func (j *journal) Seek(consumer string, to uint64) {
	if consumer == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.marks[consumer] = to
	j.saveCursorsLocked()
}

// Cursor reports where a consumer's cursor sits, with an explicit override
// taking precedence — the shape both the peeking and the consuming reads need.
func (j *journal) Cursor(consumer string, override uint64) uint64 {
	if override > 0 {
		return override
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.marks[consumer]
}

// Pending reports how many events a consumer has waiting.
func (j *journal) Pending(consumer string) int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.sinceLocked(j.marks[consumer], 0))
}

// saveCursorsLocked persists the cursors, first forgetting any that point
// before the retained window — those consumers are gone or hopelessly behind,
// and keeping them would grow the file without bound as sessions come and go.
func (j *journal) saveCursorsLocked() {
	if len(j.events) > 0 {
		oldest := j.events[0].ID
		for k, v := range j.marks {
			if v+maxJournalEvents < oldest {
				delete(j.marks, k)
			}
		}
	}
	keys := make([]string, 0, len(j.marks))
	for k := range j.marks {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Marshal through an ordered intermediate so the file is stable across
	// writes and readable when someone goes looking.
	ordered := make(map[string]uint64, len(keys))
	for _, k := range keys {
		ordered[k] = j.marks[k]
	}
	raw, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return
	}
	_ = writeFileAtomic(j.cursors, raw)
}
