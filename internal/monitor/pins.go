package monitor

import (
	"context"
	"fmt"
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
// when the tree is next due a look. What has already been *said* is not here —
// it is keyed by repository (see repoPinState), because the same checkout can
// exist several times over on one machine.
type pinState struct {
	// Path is the working tree the state belongs to.
	Path string `json:"path"`
	// NextScan is the earliest the tree may be read again. It gates the
	// filesystem work itself, not just the audit that follows, because the
	// daemon calls scanPins for every watched tree on every tick. It stays
	// per-tree even though the findings are per-repository: reading a tree is
	// what the deadline paces, and two worktrees are two directory walks.
	NextScan time.Time `json:"next_scan,omitzero"`
}

// repoPinState is the set of pin findings already reported for one repository.
//
// Keying it by repository rather than by working tree is what stops a machine
// with four worktrees of one checkout saying the same thing about the same
// stale action four times. A finding is a property of the repository's files —
// "<file>:<line>:<ref>", all repo-relative — so two worktrees on the same
// commit produce identical keys, and the second one has nothing new to say.
type repoPinState struct {
	// Repo is "<owner>/<repo>", or the working tree's path for a checkout with
	// no GitHub remote to name — which keeps the old per-tree behavior for the
	// one case where there is no repository identity to share.
	Repo string `json:"repo"`
	// Reported holds the findings already reported, keyed by repository, file,
	// line, and reference, so an unpinned action you have chosen not to fix is
	// mentioned once rather than every time you touch the file. It is ordered
	// least-recently-confirmed first, which is what the bound trims from — see
	// rememberPins.
	Reported []string `json:"reported,omitempty"`
}

// same reports whether a scan left the state exactly as it found it, so the
// daemon can skip persisting it. The deadline counts as state: a scan that ran
// and found nothing new still moved NextScan, and calling that "unchanged"
// would drop the new deadline on the floor and re-read the tree next tick.
func (st pinState) same(other pinState) bool {
	return st.Path == other.Path && st.NextScan.Equal(other.NextScan)
}

// pinRepoKey is the identity a working tree's pin findings are deduplicated
// under. The poller resolves a tree watch's owner and repo from the checkout
// itself, so it is set long before any PR is found; a tree with no GitHub
// remote at all falls back to its own path and is deduplicated alone, exactly
// as every tree used to be.
func pinRepoKey(w Watch) string {
	if w.Owner != "" && w.Repo != "" {
		return w.Owner + "/" + w.Repo
	}
	return w.Path
}

// claimPinRepo is pinRepoKey for a watch the poller may not have got to yet: a
// watch registered mid-round is audited before anything has resolved its owner,
// and keying that first audit by path would file every finding under a name the
// next round abandons — reporting the lot a second time under the repository,
// and leaving the path-keyed entry behind for good.
//
// Reading the checkout is a handful of stats that only an unresolved watch
// pays, and retarget already reads the same files for every tree every round.
// pinRepoKey itself stays free of I/O: the scope filter calls it per read.
//
// What it resolves is written back onto the watch, and that write is the point
// rather than an optimization. The scope filter builds a session's accept set
// from the registry through the I/O-free pinRepoKey, so a watch left unresolved
// claims its path and nothing else — while the findings from its own tree are
// addressed to the repository. A sibling worktree that *has* resolved then
// claims that repository key, and the event is filtered out of the feed of the
// very session whose tree produced it, with the cursor already advanced past it
// and the per-repository dedupe suppressing it ever after.
func (d *Daemon) claimPinRepo(w Watch) string {
	if w.Owner != "" && w.Repo != "" {
		return pinRepoKey(w)
	}
	checkout, err := ReadCheckout(w.Path)
	if err != nil || checkout.Owner == "" || checkout.Repo == "" {
		return pinRepoKey(w)
	}
	d.updateWatch(w.ID, func(cur *Watch) []Event {
		if cur.Owner == "" || cur.Repo == "" {
			cur.Owner, cur.Repo = checkout.Owner, checkout.Repo
		}
		return nil
	})
	w.Owner, w.Repo = checkout.Owner, checkout.Repo
	return pinRepoKey(w)
}

// scanPins audits a working tree's workflow files and returns the events for
// findings its repository has not already reported. It returns the updated
// state and the updated reported set whether or not it audited, so the caller
// can store the new deadline and compare.
//
// budget is how many keys the repository's reported set may hold — one
// maxRemembered per working tree filed under it, because the set is shared by
// every one of them and a budget that is not is one worktree's scan evicting
// another's live findings (see rememberPins).
//
// The events name the *repository*, not the tree that happened to scan it,
// because the dedupe is per repository: the second worktree of a checkout has
// nothing new to say, so if the event belonged to the first worktree's path the
// second worktree's session would never hear the finding at all (see
// watchKeys).
func (d *Daemon) scanPins(ctx context.Context, st pinState, repo string, budget int, reported []string, now time.Time) (pinState, []string, []Event) {
	if now.Before(st.NextScan) {
		return st, reported, nil
	}
	// The deadline moves before anything else can go wrong. A tree with no
	// workflows at all, or one whose .github cannot be read, has to stop
	// costing a directory walk a second just as an audited one does — and those
	// are the trees most likely to be watched for hours.
	st.NextScan = now.Add(pinScanInterval)

	files, err := pins.WorkflowFiles(st.Path)
	if err != nil || len(files) == 0 {
		return st, reported, nil
	}

	report := pins.Audit(ctx, pins.Scan(files), d.opts.PinResolver)
	if !report.HasIssues() {
		return st, reported, nil
	}

	seen := make(map[string]struct{}, len(reported))
	for _, key := range reported {
		seen[key] = struct{}{}
	}
	var events []Event
	var current []string
	for _, f := range report.Findings {
		if f.Status != pins.StatusUnpinned && f.Status != pins.StatusStale {
			continue
		}
		key := fmt.Sprintf("%s:%s:%d:%s", repo, f.File, f.Line, f.Raw)
		if _, dup := seen[key]; dup {
			// Already reported, or seen twice in this one audit: either way it
			// is still live, so it belongs in the reconciled set below.
			seen[key] = struct{}{}
			current = append(current, key)
			continue
		}
		seen[key] = struct{}{}
		current = append(current, key)
		events = append(events, pinEvent(repo, f, now))
	}
	return st, rememberPins(reported, current, budget), events
}

// rememberPins reconciles a repository's reported set against the audit that
// just ran: the keys this scan confirmed go to the end, the ones it did not
// keep their order in front of them, and only the newest budget survive.
//
// The recency order is the whole point. Nothing ever removes a key — a second
// worktree of the same repository may be on a branch where the finding still
// exists — so the set accumulates a key per line-shifting edit and per pin
// bump, and a set trimmed in any time-free order (text, say) would evict the
// same keys on every scan and re-report those findings every pinScanInterval
// forever. Trimming the least recently confirmed instead means an evicted key
// is one no recent audit has seen, so re-reporting it is news rather than a
// loop.
//
// The bound gives way to that rule rather than the other way round, in both the
// directions where it would otherwise close the loop it exists to stop:
//
//   - A key this very scan confirmed is never trimmed. A repository with more
//     than budget live findings would otherwise lose the head of the audit's
//     file/line order on every scan — keys the scan just saw, which the next
//     scan therefore reports again, forever, since recency cannot discriminate
//     between keys one audit confirmed together.
//   - The floor is at least one maxRemembered per working tree filed under the
//     repository (the caller's budget). The set is shared by every worktree of
//     a checkout, and worktrees on different branches produce different keys, so
//     a single flat bound would have each tree's scan evict the other's live
//     findings and both re-report them every pinScanInterval.
func rememberPins(previous, current []string, budget int) []string {
	live := make(map[string]struct{}, len(current))
	for _, key := range current {
		live[key] = struct{}{}
	}
	out := make([]string, 0, len(previous)+len(current))
	for _, key := range previous {
		if _, ok := live[key]; !ok {
			out = append(out, key)
		}
	}
	out = append(out, current...)
	budget = max(budget, maxRemembered, len(current))
	if len(out) > budget {
		out = out[len(out)-budget:]
	}
	return out
}

// pinEvent renders one pin finding as an event whose body is the line to
// paste. The whole value of the finding is the fix, so the fix is the body.
//
// repo is the event's target — "<owner>/<repo>", or the tree's path for a
// checkout with no remote to name. A finding is a property of the repository's
// files, and every worktree of that repository is a session that wants it.
func pinEvent(repo string, f pins.Finding, now time.Time) Event {
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
		Target: repo,
		Title:  title,
		Body:   strings.TrimRight(b.String(), "\n"),
	}
}
