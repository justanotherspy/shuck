package monitor

import (
	"fmt"
	"strings"
	"time"
)

// Kind names what happened. The set is deliberately small: an event exists
// because an agent would act differently knowing it, not because GitHub
// happened to change a field.
type Kind string

const (
	// KindCIFailed is one job that went red. Its body is the distilled
	// failing-step output — the same excerpt `shuck logs` would print.
	KindCIFailed Kind = "ci.failed"
	// KindCIPassed is every check on a head commit reaching a green terminal
	// state. It fires once per commit, and it is the event that closes the
	// push-watch-fix loop.
	KindCIPassed Kind = "ci.passed"
	// KindCIStarted is the first sighting of checks for a new head commit.
	// It exists so an agent that just pushed knows its push registered.
	KindCIStarted Kind = "ci.started"
	// KindReviewComment is a new inline review comment, distilled with its
	// diff hunk and the surrounding lines of the file at the PR head.
	KindReviewComment Kind = "review.comment"
	// KindReviewSubmitted is a submitted review — an approval, a
	// changes-requested, or a plain comment — with its inline comments
	// gathered into one event rather than scattered across several.
	KindReviewSubmitted Kind = "review.submitted"
	// KindPRState is a change to the pull request itself: opened, merged,
	// closed, or moved out of draft.
	KindPRState Kind = "pr.state"
	// KindPinsStale is a workflow file referencing an action that is not
	// SHA-pinned, or pinned to a release that has since been superseded.
	KindPinsStale Kind = "pins.stale"
	// KindTarget is the watch retargeting itself: a branch switch, a PR found
	// for the current branch, or a PR that closed out from under it.
	KindTarget Kind = "watch.target"
	// KindError is a poll that failed. Errors are reported rather than
	// swallowed, because a monitor that has quietly stopped working is worse
	// than no monitor at all.
	KindError Kind = "monitor.error"
)

// Severity ranks an event for consumers that need to decide how loudly to
// surface it — a Claude Code hook injecting context into a session, say.
type Severity string

const (
	// SeverityAction marks an event that wants the agent to do something:
	// CI went red, a reviewer asked for changes.
	SeverityAction Severity = "action"
	// SeverityInfo marks an event worth knowing but not acting on.
	SeverityInfo Severity = "info"
)

// Severity reports how much the event demands of its reader.
func (k Kind) Severity() Severity {
	switch k {
	case KindCIFailed, KindReviewComment, KindReviewSubmitted, KindPinsStale, KindError:
		return SeverityAction
	default:
		return SeverityInfo
	}
}

// Event is one thing the monitor noticed. It is the unit the journal stores,
// the IPC hands out, and the Claude Code hooks inject into a session.
//
// Title is a single line — enough to decide whether to care. Body carries the
// detail an agent needs to act without a follow-up call: the failing step's
// error excerpt, the review comment with its diff hunk, the corrected pin
// line. Splitting the two lets a consumer show a digest and expand on demand.
type Event struct {
	// ID is the journal's monotonically increasing sequence number. Consumers
	// track the last ID they were given and ask for everything after it.
	ID uint64 `json:"id"`
	// Time is when the monitor noticed, not when GitHub recorded it.
	Time time.Time `json:"time"`
	// Kind is what happened.
	Kind Kind `json:"kind"`
	// Watch is the ID of the watch that produced the event.
	Watch string `json:"watch"`
	// Target names the subject, as "owner/repo#42" or "owner/repo" when the
	// event is not about one PR.
	Target string `json:"target"`
	// Title is the one-line headline.
	Title string `json:"title"`
	// Body is the agent-ready detail; it may be empty.
	Body string `json:"body,omitempty"`
	// URL links to the thing on GitHub, when there is one to link to.
	URL string `json:"url,omitempty"`
}

// Severity reports the event's severity, derived from its kind.
func (e Event) Severity() Severity { return e.Kind.Severity() }

// Text renders one event the way a terminal reader wants it: a header line
// carrying the time, kind, and target, then the indented body.
func (e Event) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s  %s\n  %s",
		e.Time.Local().Format("15:04:05"), e.Kind, e.Target, e.Title)
	if e.URL != "" {
		fmt.Fprintf(&b, "\n  %s", e.URL)
	}
	if body := strings.TrimRight(e.Body, "\n"); body != "" {
		b.WriteString("\n")
		for line := range strings.SplitSeq(body, "\n") {
			b.WriteString("    ")
			b.WriteString(line)
			b.WriteString("\n")
		}
		return strings.TrimRight(b.String(), "\n")
	}
	return b.String()
}

// FormatFeed renders a batch of events as the block of context a Claude Code
// session is handed mid-conversation. The wording matters more than it looks:
// this text arrives unbidden in the middle of whatever the agent was doing, so
// it opens by saying where it came from, states plainly what changed, and — for
// anything actionable — says what to do about it. Without that framing an agent
// reads a wall of CI output as part of the user's request.
//
// An empty batch renders as "", so a caller can use the result's emptiness to
// decide whether to inject anything at all.
func FormatFeed(events []Event) string {
	if len(events) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<shuck-monitor>\n")
	fmt.Fprintf(&b, "The shuck background monitor observed %s since your last update.\n",
		count(len(events), "change"))
	if act := countActionable(events); act > 0 {
		fmt.Fprintf(&b, "%s below need your attention: address them as part of the current\n"+
			"task, or say why you are not going to.\n", count(act, "item"))
	}
	b.WriteString("This is monitor output, not a message from the user.\n")

	for _, e := range events {
		b.WriteString("\n")
		b.WriteString(e.Text())
		b.WriteString("\n")
	}
	b.WriteString("</shuck-monitor>")
	return b.String()
}

// countActionable counts the events in a batch that ask something of the
// reader.
func countActionable(events []Event) int {
	n := 0
	for _, e := range events {
		if e.Severity() == SeverityAction {
			n++
		}
	}
	return n
}

// count renders "1 change" / "4 changes" for a regular noun.
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
