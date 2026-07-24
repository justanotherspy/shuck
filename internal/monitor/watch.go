package monitor

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WatchKind distinguishes the two ways of saying what you care about.
type WatchKind string

const (
	// WatchTree follows a working tree. It is the interesting one: the watch
	// holds a directory, and the monitor re-reads that tree's repository and
	// branch on every tick, so switching branches or worktrees retargets it
	// without anyone saying so.
	WatchTree WatchKind = "tree"
	// WatchPR pins one pull request by number, for the times you want to keep
	// an eye on something you are not checked out on.
	WatchPR WatchKind = "pr"
)

// Watch is one thing the monitor follows.
type Watch struct {
	// ID is stable and derived from what the watch names, so adding the same
	// tree or PR twice updates the existing watch instead of duplicating it.
	ID   string    `json:"id"`
	Kind WatchKind `json:"kind"`
	// Path is the working tree, for a tree watch.
	Path string `json:"path,omitempty"`
	// Owner, Repo and Number identify the PR. For a tree watch they are the
	// last resolution, refreshed as the tree moves; Number is 0 until a PR is
	// found for the branch.
	Owner  string `json:"owner,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
	// Branch is the tree's current branch, for a tree watch.
	Branch string `json:"branch,omitempty"`
	// Note explains why a watch is not currently resolving to a PR — a
	// detached HEAD, no open PR for the branch, an unreadable working tree.
	Note string `json:"note,omitempty"`

	Added time.Time `json:"added"`
	// Resolved is when the branch was last looked up against the repository's
	// open pull requests. A branch with no PR must not be re-looked-up on
	// every tick — that would be a request a second for a question whose
	// answer changes at human speed.
	Resolved time.Time `json:"resolved,omitzero"`
	// LastSeen is refreshed whenever a client asks about this watch. A watch
	// nobody has asked about for DefaultWatchTTL is dropped, which is how the
	// monitor stops working on a laptop whose sessions have all ended.
	LastSeen time.Time `json:"last_seen"`
}

// TreeWatchID derives a tree watch's ID from its path.
func TreeWatchID(path string) string { return "tree:" + path }

// PRWatchID derives a PR watch's ID from its target.
func PRWatchID(owner, repo string, number int) string {
	return fmt.Sprintf("pr:%s/%s#%d", owner, repo, number)
}

// Target renders the watch's current subject as "owner/repo#42", or
// "owner/repo" when no PR is resolved, or "" when even the repository is
// unknown. It is both the display form and the key the poller groups by, so
// two watches that land on the same PR are polled once between them.
func (w Watch) Target() string {
	if w.Owner == "" || w.Repo == "" {
		return ""
	}
	if w.Number == 0 {
		return w.Owner + "/" + w.Repo
	}
	return fmt.Sprintf("%s/%s#%d", w.Owner, w.Repo, w.Number)
}

// Describe renders the watch for `shuck monitor status`: what it follows, what
// that currently resolves to, and why not when it does not.
func (w Watch) Describe() string {
	var b strings.Builder
	switch w.Kind {
	case WatchTree:
		b.WriteString(w.Path)
	default:
		b.WriteString(w.Target())
	}
	if w.Kind == WatchTree {
		switch {
		case w.Owner == "":
			b.WriteString(" — not resolved yet")
		case w.Number > 0:
			fmt.Fprintf(&b, " → %s (%s)", w.Target(), w.Branch)
		default:
			fmt.Fprintf(&b, " → %s/%s on %s", w.Owner, w.Repo, w.Branch)
		}
	}
	if w.Note != "" {
		fmt.Fprintf(&b, " [%s]", w.Note)
	}
	return b.String()
}

// ParseWatchSpec interprets the argument to `shuck monitor watch`. It accepts
// everything the rest of shuck accepts for naming a PR — a URL, an
// "owner/repo 42" pair, "owner/repo#42" — plus a directory, which becomes a
// tree watch. With no argument at all the caller's working directory is the
// tree, which is the case the whole design is built around.
func ParseWatchSpec(args []string, cwd string) (Watch, error) {
	now := time.Now()
	if len(args) == 0 {
		abs, err := filepath.Abs(cwd)
		if err != nil {
			return Watch{}, fmt.Errorf("resolve working directory: %w", err)
		}
		return Watch{ID: TreeWatchID(abs), Kind: WatchTree, Path: abs, Added: now, LastSeen: now}, nil
	}

	owner, repo, number, err := parsePRSpec(args)
	if err != nil {
		return Watch{}, err
	}
	return Watch{
		ID:       PRWatchID(owner, repo, number),
		Kind:     WatchPR,
		Owner:    owner,
		Repo:     repo,
		Number:   number,
		Added:    now,
		LastSeen: now,
	}, nil
}

// parsePRSpec pulls owner, repo, and PR number out of the accepted spellings.
func parsePRSpec(args []string) (owner, repo string, number int, err error) {
	joined := strings.Join(args, " ")
	if len(args) == 1 {
		if o, r, n, ok := splitHashSpec(args[0]); ok {
			return o, r, n, nil
		}
	}
	// target.Resolve handles URLs and the "owner/repo 42" pair, and falls back
	// to the local repository for a bare number — which is exactly the
	// behavior we want, since a bare number means "this repo, that PR".
	tgt, err := resolveTarget(args)
	if err != nil {
		return "", "", 0, err
	}
	if tgt.Number == 0 {
		return "", "", 0, fmt.Errorf("%q does not name a pull request; pass owner/repo#42, a PR URL, or a directory to follow", joined)
	}
	return tgt.Owner, tgt.Repo, tgt.Number, nil
}

// splitHashSpec parses the "owner/repo#42" shorthand, which target.Resolve does
// not accept but which is the form the monitor prints, so it must round-trip.
func splitHashSpec(s string) (owner, repo string, number int, ok bool) {
	slug, num, found := strings.Cut(s, "#")
	if !found {
		return "", "", 0, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", "", 0, false
	}
	o, r, found := strings.Cut(slug, "/")
	if !found || o == "" || r == "" || strings.Contains(r, "/") {
		return "", "", 0, false
	}
	return o, r, n, true
}

// registry is the daemon's set of watches, persisted so a restart resumes
// following the same things.
type registry struct {
	path    string
	watches map[string]*Watch
}

// loadRegistry reads the persisted watch set. A missing or unreadable file
// yields an empty registry: losing the watch list costs a session one
// `monitor watch` call, whereas refusing to start costs it the monitor.
func loadRegistry(p paths) *registry {
	r := &registry{path: p.watches, watches: map[string]*Watch{}}
	var stored []Watch
	if readJSONFile(p.watches, &stored) == nil {
		for i := range stored {
			w := stored[i]
			if w.ID != "" {
				r.watches[w.ID] = &w
			}
		}
	}
	return r
}

// Add inserts or refreshes a watch and returns the stored copy. Re-adding an
// existing watch keeps whatever the poller has already resolved about it and
// just marks it as seen, so a session restarting does not reset its state.
func (r *registry) Add(w Watch) *Watch {
	if existing, ok := r.watches[w.ID]; ok {
		existing.LastSeen = time.Now()
		r.save()
		return existing
	}
	if w.Added.IsZero() {
		w.Added = time.Now()
	}
	w.LastSeen = time.Now()
	r.watches[w.ID] = &w
	r.save()
	return &w
}

// Remove drops a watch, reporting whether it was there.
func (r *registry) Remove(id string) bool {
	_, ok := r.watches[id]
	delete(r.watches, id)
	if ok {
		r.save()
	}
	return ok
}

// List returns the watches in a stable order.
func (r *registry) List() []Watch {
	out := make([]Watch, 0, len(r.watches))
	for _, w := range r.watches {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns the live watch for an ID.
func (r *registry) Get(id string) (*Watch, bool) {
	w, ok := r.watches[id]
	return w, ok
}

// Touch marks watches as seen. Clients call it for the watches they ask about,
// which is what keeps a watch alive while a session is using it.
func (r *registry) Touch(ids ...string) {
	now := time.Now()
	changed := false
	for _, id := range ids {
		if w, ok := r.watches[id]; ok {
			w.LastSeen = now
			changed = true
		}
	}
	if changed {
		r.save()
	}
}

// TouchAll marks every watch as seen, for the client calls that are about the
// monitor as a whole rather than one watch.
func (r *registry) TouchAll() {
	now := time.Now()
	for _, w := range r.watches {
		w.LastSeen = now
	}
	if len(r.watches) > 0 {
		r.save()
	}
}

// Expire drops watches nobody has asked about within ttl and returns them.
func (r *registry) Expire(ttl time.Duration, now time.Time) []Watch {
	if ttl <= 0 {
		return nil
	}
	var dropped []Watch
	for id, w := range r.watches {
		if now.Sub(w.LastSeen) > ttl {
			dropped = append(dropped, *w)
			delete(r.watches, id)
		}
	}
	if len(dropped) > 0 {
		sort.Slice(dropped, func(i, j int) bool { return dropped[i].ID < dropped[j].ID })
		r.save()
	}
	return dropped
}

// Len reports how many watches are registered.
func (r *registry) Len() int { return len(r.watches) }

// save persists the watch set. A failure is not fatal — the in-memory set is
// authoritative for this process, and the only cost is that a restart forgets.
func (r *registry) save() {
	if r.path == "" {
		return
	}
	_ = writeJSONFile(r.path, r.List())
}
