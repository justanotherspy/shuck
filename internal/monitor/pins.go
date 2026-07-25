package monitor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/justanotherspy/shuck/internal/pins"
)

// pinScanInterval paces the pin audit of one working tree — both the reading of
// its workflow files and the release lookups that follow.
//
// Both halves need pacing, and the reading needs it most. The daemon wakes
// every second, and every watched tree would otherwise be walked and read on
// every one of those ticks, for as long as a session lasts, to learn something
// that only changes when a human edits a file. Resolving an action's latest
// release is a network call on top of that.
//
// Ten minutes is that human pace. An edit lands on the next scan rather than on
// the next keystroke, and a tree nobody has touched is still re-audited: an
// action can cut a release without anyone editing this repo, and a pin goes
// stale exactly then.
const pinScanInterval = 10 * time.Minute

// pinState is what the monitor remembers about one working tree's pin audit:
// when the tree is next due a look, and what has already been said about it.
type pinState struct {
	// Path is the working tree the state belongs to.
	Path string `json:"path"`
	// NextScan is the earliest the tree may be read again. It gates the
	// filesystem work itself, not just the audit that follows, because the
	// daemon calls scanPins for every watched tree on every tick.
	NextScan time.Time `json:"next_scan,omitzero"`
	// Reported holds the findings already reported, keyed by file, line, and
	// reference, so an unpinned action you have chosen not to fix is mentioned
	// once rather than every time you touch the file.
	Reported []string `json:"reported,omitempty"`
}

// same reports whether a scan left the state exactly as it found it, so the
// daemon can skip persisting it. The deadline counts as state: a scan that ran
// and found nothing new still moved NextScan, and calling that "unchanged"
// would drop the new deadline on the floor and re-read the tree next tick.
func (st pinState) same(other pinState) bool {
	return st.Path == other.Path &&
		st.NextScan.Equal(other.NextScan) &&
		slices.Equal(st.Reported, other.Reported)
}

// scanPins audits a working tree's workflow files and returns the events for
// findings not already reported. It returns the updated state whether or not it
// audited, so the caller can store the new deadline.
func (d *Daemon) scanPins(ctx context.Context, st pinState, now time.Time) (pinState, []Event) {
	if now.Before(st.NextScan) {
		return st, nil
	}
	// The deadline moves before anything else can go wrong. A tree with no
	// workflows at all, or one whose .github cannot be read, has to stop
	// costing a directory walk a second just as an audited one does — and those
	// are the trees most likely to be watched for hours.
	st.NextScan = now.Add(pinScanInterval)

	files, err := pins.WorkflowFiles(st.Path)
	if err != nil || len(files) == 0 {
		return st, nil
	}

	report := pins.Audit(ctx, pins.Scan(files), d.opts.PinResolver)
	if !report.HasIssues() {
		return st, nil
	}

	// Pin keys are "<file>:<line>:<ref>" — there is no time in them, so no
	// ordering can name the "oldest" finding. Text order at least keeps the
	// persisted file stable; on the (unlikely) overflow of a repo with more
	// than maxRemembered findings, the cost is one finding mentioned twice.
	seen := newStringSet(st.Reported, strings.Compare)
	var events []Event
	for _, f := range report.Findings {
		if f.Status != pins.StatusUnpinned && f.Status != pins.StatusStale {
			continue
		}
		key := fmt.Sprintf("%s:%d:%s", f.File, f.Line, f.Raw)
		if seen.has(key) {
			continue
		}
		seen.add(key)
		events = append(events, pinEvent(st.Path, f, now))
	}
	st.Reported = seen.slice()
	return st, events
}

// pinEvent renders one pin finding as an event whose body is the line to
// paste. The whole value of the finding is the fix, so the fix is the body.
func pinEvent(path string, f pins.Finding, now time.Time) Event {
	title := fmt.Sprintf("%s:%d uses %s, which is not SHA-pinned", f.File, f.Line, f.Raw)
	if f.Status == pins.StatusStale {
		title = fmt.Sprintf("%s:%d pins %s, but %s is newer", f.File, f.Line, f.Comment, f.Latest)
	}

	var b strings.Builder
	if f.Note != "" {
		b.WriteString(f.Note)
		b.WriteString("\n")
	}
	if f.PinLine != "" {
		fmt.Fprintf(&b, "Replace the reference with:\n  uses: %s", f.PinLine)
	}
	return Event{
		Time:   now,
		Kind:   KindPinsStale,
		Target: path,
		Title:  title,
		Body:   strings.TrimRight(b.String(), "\n"),
	}
}
