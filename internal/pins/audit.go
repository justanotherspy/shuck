package pins

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/justanotherspy/shuck/internal/action"
	"github.com/justanotherspy/shuck/internal/semver"
)

// Status classifies one Use against the latest release of its action.
type Status int

// Per-reference outcomes, ordered from best to worst.
const (
	StatusPinned   Status = iota // SHA-pinned and current
	StatusStale                  // SHA-pinned but a newer release exists
	StatusUnpinned               // a mutable tag/branch ref
	StatusSkipped                // local/docker ref, or resolution unavailable
)

// String returns the lowercase name of the status, used in the JSON view.
func (s Status) String() string {
	switch s {
	case StatusPinned:
		return "pinned"
	case StatusStale:
		return "stale"
	case StatusUnpinned:
		return "unpinned"
	case StatusSkipped:
		return "skipped"
	default:
		return "unknown"
	}
}

// Finding is one audited `uses:` reference: the reference as found, what the
// audit concluded, and — when shuck knows one — the exact replacement line to
// paste over it.
type Finding struct {
	Use
	Status  Status
	Latest  string // latest release tag, when known
	SHA     string // the commit SHA of Latest, when known
	PinLine string // the corrected "owner/action@<sha> # <tag>" line, when a fix is known
	Note    string // why it was skipped, or what changed
}

// Report is a whole-repository pin audit. Root is the repository the findings
// came from; Audit itself only sees references, so a caller that scanned a
// directory fills it in (Repository does).
type Report struct {
	Root      string
	Findings  []Finding
	Unpinned  int
	Stale     int
	Skipped   int
	CheckedAt time.Time
}

// Count tallies the findings in the given status.
func (r Report) Count(status Status) int {
	n := 0
	for _, f := range r.Findings {
		if f.Status == status {
			n++
		}
	}
	return n
}

// HasIssues reports whether any reference is unpinned or behind its latest
// release. A skipped reference is not an issue on its own — shuck could not
// judge it, which is never grounds for failing a build.
func (r Report) HasIssues() bool { return r.Unpinned > 0 || r.Stale > 0 }

// Resolver resolves an action slug (with an optional version constraint) to its
// latest matching release. The caller supplies it — the gh-backed one in the
// CLI, a fake in tests — so this package stays pure and offline-testable.
type Resolver interface {
	Resolve(ctx context.Context, ref action.Ref) (action.Resolved, error)
}

// Audit classifies every use against the resolver and returns the findings
// sorted by file then line.
//
// Nothing a resolver does can abort the audit: a resolution failure degrades
// that one reference to StatusSkipped with the error in its Note, and a nil
// Resolver simply skips every remote reference — which makes Audit usable as a
// pure offline "what does this repo use" pass.
func Audit(ctx context.Context, uses []Use, r Resolver) Report {
	cache := &resolveCache{resolver: r, seen: map[string]resolution{}}
	rep := Report{
		Findings:  make([]Finding, 0, len(uses)),
		CheckedAt: time.Now().UTC(),
	}
	for _, u := range uses {
		rep.Findings = append(rep.Findings, auditUse(ctx, u, cache))
	}
	sort.SliceStable(rep.Findings, func(i, j int) bool {
		if rep.Findings[i].File != rep.Findings[j].File {
			return rep.Findings[i].File < rep.Findings[j].File
		}
		return rep.Findings[i].Line < rep.Findings[j].Line
	})
	rep.Unpinned = rep.Count(StatusUnpinned)
	rep.Stale = rep.Count(StatusStale)
	rep.Skipped = rep.Count(StatusSkipped)
	return rep
}

// Repository is the one-call form the CLI and the workflow monitor use: collect
// root's workflow files, scan them, audit the references, and label the report
// with the root it came from.
func Repository(ctx context.Context, root string, r Resolver) (Report, error) {
	files, err := WorkflowFiles(root)
	if err != nil {
		return Report{}, fmt.Errorf("collect workflow files in %s: %w", root, err)
	}
	rep := Audit(ctx, Scan(files), r)
	rep.Root = root
	return rep, nil
}

// auditUse classifies a single reference. Everything that is not a remote
// action is skipped with a note explaining why there is nothing to pin.
func auditUse(ctx context.Context, u Use, cache *resolveCache) Finding {
	f := Finding{Use: u, Status: StatusSkipped}
	switch u.Kind {
	case UseInvalid:
		f.Note = noteOr(u.Err, "the file could not be parsed as YAML")
		return f
	case UseLocal:
		f.Note = "local action reference — it ships with this repository, nothing to pin"
		return f
	case UseDocker:
		f.Note = "docker image reference — pin its digest with `shuck image`"
		return f
	case UseRemote:
	}

	base, err := action.ParseRef(u.Slug)
	if err != nil {
		f.Note = fmt.Sprintf("cannot read the action reference: %v", err)
		return f
	}
	if IsSHA(u.Ref) {
		return auditPinned(ctx, f, base, cache)
	}
	return auditUnpinned(ctx, f, base, cache)
}

// auditUnpinned handles a reference that is not a commit SHA — a tag, a branch,
// or nothing at all. The suggested pin stays on whatever major the author
// chose (`@v4` resolves within 4.x.x), because silently proposing a major bump
// would turn a pinning fix into a behavior change. A ref that is not semver at
// all (a branch like `main`) has no major to honor, so it resolves to the
// latest release overall.
func auditUnpinned(ctx context.Context, f Finding, base action.Ref, cache *resolveCache) Finding {
	constraint := ""
	if _, ok := semver.ParseConstraint(f.Ref); ok {
		constraint = f.Ref
	}

	// Whether a reference is pinned is a property of the reference itself, not
	// of whether the latest release could be looked up. A resolver failure
	// costs the finding its suggested fix; it must not cost the finding.
	f.Status = StatusUnpinned

	res, err := cache.resolve(ctx, base, constraint)
	if err != nil {
		f.Note = fmt.Sprintf("%q is not a commit SHA; the pin to use could not be resolved: %v", f.Ref, err)
		return f
	}

	f.Latest, f.SHA, f.PinLine = res.Tag, res.SHA, res.PinLine()
	switch {
	case f.Ref == "":
		f.Note = "no version at all — the step runs whatever is on the action's default branch"
	case constraint == "":
		f.Note = fmt.Sprintf("%q is a branch or non-semver tag — it can be moved to any commit", f.Ref)
	default:
		f.Note = fmt.Sprintf("%q is a mutable tag — each release re-points it", f.Ref)
	}
	return f
}

// auditPinned handles a reference that already carries a commit SHA. The SHA
// itself says nothing about which release it is, so the version comment shuck
// (and this repository) writes after the pin — `owner/action@<sha> # v4.2.2` —
// is the only thing staleness can be judged against; without it the pin is
// reported as current with a note asking for the comment.
//
// The constraint used to resolve is the comment's MAJOR only. A pin says "I
// chose v4", so the useful answer is the newest v4.x.x, not a v5 that would
// change behavior. A newer major is still worth knowing about, so it is looked
// up separately and appended to the note rather than driving the suggested pin.
func auditPinned(ctx context.Context, f Finding, base action.Ref, cache *resolveCache) Finding {
	pinned, ok := commentVersion(f.Comment)
	if !ok {
		f.Status = StatusPinned
		f.Note = "no version comment — add `# <tag>` after the SHA so staleness can be checked"
		return f
	}

	res, err := cache.resolve(ctx, base, "v"+strconv.Itoa(pinned.Major))
	if err != nil {
		f.Status = StatusSkipped
		f.Note = err.Error()
		return f
	}
	f.Latest, f.SHA = res.Tag, res.SHA

	latest, latestOK := semver.Parse(res.Tag)
	switch {
	case latestOK && semver.Compare(latest, pinned) > 0:
		f.Status = StatusStale
		f.PinLine = res.PinLine()
		f.Note = fmt.Sprintf("%s → %s", pinned.Raw, res.Tag)
	case res.SHA != "" && !strings.EqualFold(res.SHA, f.Ref):
		// Same tag, different commit: the release was moved after it shipped, so
		// the pinned SHA is no longer what that tag means.
		f.Status = StatusStale
		f.PinLine = res.PinLine()
		f.Note = fmt.Sprintf("%s was re-tagged — it now points at %s", res.Tag, res.SHA)
	default:
		f.Status = StatusPinned
	}

	if tag, ok := cache.newerMajor(ctx, base, pinned); ok {
		f.Note = joinNotes(f.Note, fmt.Sprintf("a newer major %s is available", tag))
	}
	return f
}

// commentVersion reads the semver tag out of a pin's trailing comment. Only the
// first whitespace-separated word is considered, so a comment that adds prose
// after the tag ("# v4.2.2 pinned by shuck") still parses.
func commentVersion(comment string) (semver.Version, bool) {
	word, _, _ := strings.Cut(strings.TrimSpace(comment), " ")
	if word == "" {
		return semver.Version{}, false
	}
	return semver.Parse(word)
}

// resolution memoizes one resolver answer, error included, so a failing action
// is not retried once per workflow that uses it.
type resolution struct {
	res action.Resolved
	err error
}

// resolveCache de-duplicates resolver calls per slug+constraint. Large
// repositories use the same handful of actions across every workflow, so
// without this the audit would ask the network the same question dozens of
// times.
type resolveCache struct {
	resolver Resolver
	seen     map[string]resolution
}

// resolve returns the latest release of base matching constraint, memoized.
// The returned error is already phrased for a finding's Note.
func (c *resolveCache) resolve(ctx context.Context, base action.Ref, constraint string) (action.Resolved, error) {
	ref := base
	ref.Constraint = constraint
	key := ref.Slug() + "@" + constraint
	if got, ok := c.seen[key]; ok {
		return got.res, got.err
	}

	var got resolution
	if c.resolver == nil {
		got.err = fmt.Errorf("no resolver configured — %s was not checked against its releases", ref.Slug())
	} else {
		got.res, got.err = c.resolver.Resolve(ctx, ref)
		if got.err != nil {
			got.err = fmt.Errorf("cannot resolve the latest release of %s: %w", key, got.err)
		}
	}
	c.seen[key] = got
	return got.res, got.err
}

// newerMajor reports the latest release overall when it is a major above the
// pinned version. A failure here is not a finding — it only costs the extra
// hint — so the error is dropped.
func (c *resolveCache) newerMajor(ctx context.Context, base action.Ref, pinned semver.Version) (string, bool) {
	res, err := c.resolve(ctx, base, "")
	if err != nil {
		return "", false
	}
	latest, ok := semver.Parse(res.Tag)
	if !ok || latest.Major <= pinned.Major {
		return "", false
	}
	return res.Tag, true
}

// joinNotes appends an extra note to an existing one, keeping either alone
// readable when the other is empty.
func joinNotes(note, extra string) string {
	switch {
	case note == "":
		return extra
	case extra == "":
		return note
	default:
		return note + "; " + extra
	}
}

// noteOr returns s, or fallback when s is empty.
func noteOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
