package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/justanotherspy/shuck/internal/pins"
)

// pinScanInterval is the floor between two pin audits of the same working
// tree. The scan itself is local and instant, but resolving an action's latest
// release is a network call, so a tree whose workflows are being edited
// repeatedly is re-audited at a human pace rather than a keystroke one.
const pinScanInterval = 10 * time.Minute

// pinState is what the monitor remembers about one working tree's workflow
// files: a fingerprint of their contents and when they were last audited.
//
// The fingerprint is what makes this cheap. Reading a handful of small YAML
// files every tick costs nothing; asking GitHub about every action they
// reference does not. So the files are hashed on each tick and the audit only
// runs when the hash moves — which is to say, exactly when you have just
// written or edited a workflow.
type pinState struct {
	// Path is the working tree the state belongs to.
	Path string `json:"path"`
	// Digest fingerprints the workflow files as of the last audit.
	Digest string `json:"digest,omitempty"`
	// LastAudit is when the audit last ran.
	LastAudit time.Time `json:"last_audit,omitzero"`
	// Reported holds the findings already reported, keyed by file, line, and
	// reference, so an unpinned action you have chosen not to fix is mentioned
	// once rather than every time you touch the file.
	Reported []string `json:"reported,omitempty"`
}

// scanPins audits a working tree's workflow files and returns the events for
// findings not already reported. It returns the updated state whether or not it
// audited, so the caller can store the new fingerprint.
func (d *Daemon) scanPins(ctx context.Context, st pinState, now time.Time) (pinState, []Event) {
	files, err := pins.WorkflowFiles(st.Path)
	if err != nil || len(files) == 0 {
		return st, nil
	}

	digest := digestFiles(files)
	if digest == st.Digest && now.Sub(st.LastAudit) < pinScanInterval {
		return st, nil
	}
	// A tree whose workflows have not changed is still re-audited once an
	// interval: an action can cut a release without anyone touching this repo,
	// and a pin goes stale exactly then.
	st.Digest = digest
	st.LastAudit = now

	report := pins.Audit(ctx, pins.Scan(files), d.opts.PinResolver)
	if !report.HasIssues() {
		return st, nil
	}

	seen := newStringSet(st.Reported)
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

// digestFiles fingerprints a set of workflow files, contents and names both, so
// a rename registers as a change just as an edit does.
func digestFiles(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(files[name]))
		h.Write(files[name])
	}
	return hex.EncodeToString(h.Sum(nil))
}
