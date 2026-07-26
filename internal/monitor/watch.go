package monitor

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	// Scopes are the working trees whose sessions this watch belongs to: its
	// own path, for a tree watch, plus the directory each process that
	// registered it was working in. The event feed is filtered by them, which
	// is what stops a session in one worktree being handed another worktree's
	// CI — and what makes a PR an agent added by hand ("shuck monitor watch
	// owner/repo#42") belong to the session that added it.
	//
	// It is a set rather than one path because a watch id names what is
	// watched, not who asked: two trees can register the same PR, and a single
	// field would have the second registrant either steal the watch from the
	// first or be ignored by it — one of the two sessions silently getting
	// nothing either way.
	//
	// Empty means "belongs to everyone", which is what a watch persisted by a
	// daemon that predates this field decodes to. Delivering one watch's
	// events too widely costs noise; dropping them costs the failure nobody
	// then hears about, so the empty set fails toward delivery.
	Scopes []string `json:"scopes,omitempty"`

	Added time.Time `json:"added"`
	// Resolved is when the branch was last looked up against the repository's
	// open pull requests. A branch with no PR must not be re-looked-up on
	// every tick — that would be a request a second for a question whose
	// answer changes at human speed.
	Resolved time.Time `json:"resolved,omitzero"`
	// LastSeen is refreshed by the ops that ask how things stand — status,
	// events and poke — and each of them marks every watch, not the one it
	// named, since a session asking anything is evidence somebody is still
	// there. Registering a watch marks that one. A set that goes
	// DefaultWatchTTL without any of those is dropped, which is how the monitor
	// stops working on a laptop whose sessions have all ended.
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

// maxWatchScopes bounds how many sessions one watch remembers. The set is
// persisted and a watch id is stable, so without a cap a long-lived PR watch
// would accumulate an entry per directory anyone ever registered it from. Eight
// is far past the number of working trees that share one watch in practice, and
// the least recently registered is what a ninth displaces.
//
// Least recently *registered* rather than first seen, because nothing
// re-registers a PR watch on its own: the hooks and the stream both register
// the session's tree, so a PR an agent added by hand is only ever re-registered
// by hand. Evicting in arrival order would drop the scope of a session that has
// kept asking for the watch since the day it was added, and it would then be
// filtered out of the feed for a PR it explicitly asked to follow, with nothing
// in `shuck monitor status` to say so.
const maxWatchScopes = 8

// AddSessionScope records that a Claude Code session owns this watch, so the
// stream serving that session is handed its events wherever the session has
// since moved. It is a no-op for an empty id, which is every caller that is not
// running under Claude Code.
func (w *Watch) AddSessionScope(id string) {
	w.addScope(SessionScope(id))
}

// addScope records that the session working in scope registered this watch,
// moving a scope that is already there to the back of the eviction queue.
func (w *Watch) addScope(scope string) {
	if scope == "" {
		return
	}
	if i := slices.Index(w.Scopes, scope); i >= 0 {
		w.Scopes = slices.Delete(w.Scopes, i, i+1)
	}
	w.Scopes = append(w.Scopes, scope)
	if len(w.Scopes) > maxWatchScopes {
		w.Scopes = w.Scopes[len(w.Scopes)-maxWatchScopes:]
	}
}

// InScope reports whether this watch belongs to the session working in scope.
func (w Watch) InScope(scope string) bool { return w.inScope(resolveScope(scope)) }

// resolveScope canonicalizes a scope for comparison: a session scope is already
// canonical and must be left alone, and anything else is a path.
func resolveScope(scope string) string {
	if isSessionScope(scope) {
		return scope
	}
	return resolveTree(scope)
}

// sessionScopePrefix marks a scope that names a Claude Code session rather than
// a directory. The two cannot share a namespace: every path scope is compared
// after resolveTree, which makes a bare session id relative to whichever process
// asked, and the daemon's working directory is never the session's.
const sessionScopePrefix = "session:"

// SessionScope is the scope value standing for a Claude Code session id.
//
// It is what lets a watch belong to a *session* instead of to the directory that
// session happened to open in. A path scope is fixed at the moment it is
// recorded, so a session that starts outside a repository — the Claude Code
// agent-fleet pattern, opened in a parent directory and then moved into a
// checkout — can never be reached by one: the stream registered a directory that
// resolves to no repository, and every watch added afterwards carries a path the
// stream does not ask with. Tagging both the watch and the stream with the
// session id instead means the retarget needs nothing to restart. The hook that
// sees the new directory registers it under the same session, and the stream
// already blocked on a read is delivering it on the next tick.
func SessionScope(id string) string {
	if id == "" {
		return ""
	}
	return sessionScopePrefix + id
}

// isSessionScope reports whether a scope names a session rather than a path.
func isSessionScope(scope string) bool {
	return strings.HasPrefix(scope, sessionScopePrefix)
}

// inAnyScope reports whether this watch belongs to any of several already-
// resolved identities — a caller's directory and the session it belongs to.
// A watch with no scopes at all belongs to everyone, exactly as in inScope.
func (w Watch) inAnyScope(resolved []string) bool {
	if len(w.Scopes) == 0 {
		return true
	}
	return slices.ContainsFunc(resolved, w.inScope)
}

// inScope is InScope against an already-resolved path, so a filter over the
// whole registry resolves the asking session's directory once rather than once
// per watch.
//
// Matching is exact — equal after symlinks are resolved — and deliberately not
// the containment match StreamServes uses. A git worktree in this repository
// lives at <repo>/.claude/worktrees/<name>, inside the main checkout, so
// containment would make the worktree's session and the main checkout's session
// each other's scope and reintroduce exactly the cross-talk this exists to stop.
//
// Session scopes bypass the path comparison entirely and match as opaque
// strings. Running them through resolveTree would turn "session:<uuid>" into a
// path under whichever process asked, so the daemon and the stream would derive
// two different absolute paths from one id and never match.
func (w Watch) inScope(resolved string) bool {
	if len(w.Scopes) == 0 {
		return true
	}
	if resolved == "" {
		return false
	}
	for _, s := range w.Scopes {
		if isSessionScope(s) || isSessionScope(resolved) {
			if s == resolved {
				return true
			}
			continue
		}
		if resolveTree(s) == resolved {
			return true
		}
	}
	return false
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
//
// The PR spellings are tried first and a directory is only the fallback. The
// other order would let a directory that happens to be named "42" shadow PR
// #42 — a watch pointed somewhere other than where the person said, with
// nothing in the output to give it away. Nothing is lost by the ordering: no
// path is a spelling parsePRSpec accepts, so it fails on one cleanly.
// cwd is also what scopes the watch to the session registering it: a PR watch
// belongs to whoever asked for it, and a tree watch belongs both to the tree it
// follows and to the directory it was asked for from. A cwd that cannot be
// resolved simply leaves the watch unscoped, which is the pre-scoping behavior
// and costs noise rather than delivery.
func ParseWatchSpec(args []string, cwd string) (Watch, error) {
	now := time.Now()
	scope := ""
	if cwd != "" {
		if abs, err := filepath.Abs(cwd); err == nil {
			scope = abs
		}
	}
	if len(args) == 0 {
		abs, err := filepath.Abs(cwd)
		if err != nil {
			return Watch{}, fmt.Errorf("resolve working directory: %w", err)
		}
		return treeWatch(abs, now, abs), nil
	}

	owner, repo, number, err := parsePRSpec(args)
	if err != nil {
		if w, ok := treeWatchArg(args, now, scope); ok {
			return w, nil
		}
		// The PR failure is the one worth reporting: it already names every
		// spelling that would have worked, the directory included.
		return Watch{}, err
	}
	w := Watch{
		ID:       PRWatchID(owner, repo, number),
		Kind:     WatchPR,
		Owner:    owner,
		Repo:     repo,
		Number:   number,
		Added:    now,
		LastSeen: now,
	}
	w.addScope(scope)
	// And the checkout that directory sits in. Scopes are matched exactly, and
	// the only directory a session ever asks with is the one it opened in — so
	// a PR watch added from a subdirectory ("cd internal/cli && shuck monitor
	// watch owner/repo#42") would otherwise be scoped to a path no stream or
	// hook ever sends, and every failure on the PR the agent explicitly asked
	// to follow would be claimed by a watch nobody owns and delivered nowhere.
	if scope != "" {
		if root, ok := WorkTreeRoot(scope); ok {
			w.addScope(root)
		}
	}
	return w, nil
}

// treeWatchArg reads a single argument as a working tree to follow, reporting
// whether it is one. Only a directory that exists qualifies: a typo'd PR
// spelling must come back as the parse error the person can act on, not as a
// watch on a path that is not there.
func treeWatchArg(args []string, now time.Time, scope string) (Watch, bool) {
	if len(args) != 1 {
		return Watch{}, false
	}
	info, err := os.Stat(args[0])
	if err != nil || !info.IsDir() {
		return Watch{}, false
	}
	// Absolute, because the watch outlives the shell that named it: the daemon
	// re-reads this path on every tick, from its own working directory.
	abs, err := filepath.Abs(args[0])
	if err != nil {
		return Watch{}, false
	}
	return treeWatch(abs, now, scope), true
}

// treeWatch builds a watch on the working tree at an absolute path. It is
// scoped to that tree and to the tree it was asked for from, so `shuck monitor
// watch /some/other/checkout` still reaches the session that typed it.
func treeWatch(path string, now time.Time, scope string) Watch {
	w := Watch{ID: TreeWatchID(path), Kind: WatchTree, Path: path, Added: now, LastSeen: now}
	w.addScope(path)
	w.addScope(scope)
	return w
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
//
// The registrant's scopes are the one thing merged in rather than discarded. A
// watch id says what is watched, so a second session in a second worktree — or
// the same session against a watch a daemon of an older build persisted without
// any scope at all — arrives here as an existing watch, and keeping the stored
// copy verbatim would leave that session filtered out of its own feed forever.
func (r *registry) Add(w Watch) *Watch {
	if existing, ok := r.watches[w.ID]; ok {
		for _, scope := range w.Scopes {
			existing.addScope(scope)
		}
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

// TouchAll marks every watch as seen. The three ops that ask how things stand —
// status, events, poke — call it, and they refresh the whole set rather than the
// watch the question named, because which watch somebody asked about says
// nothing about whether the others still matter. The teardown ops do not: seek,
// unwatch and stop are a session leaving, and ping is a client deciding whether
// there is a daemon at all. Registering a watch refreshes that one, in Add.
func (r *registry) TouchAll() {
	now := time.Now()
	for _, w := range r.watches {
		w.LastSeen = now
	}
	if len(r.watches) > 0 {
		r.save()
	}
}

// Expire drops watches whose LastSeen has gone ttl without a refresh, and
// returns them. See LastSeen for which calls refresh it.
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
