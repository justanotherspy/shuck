package monitor

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/target"
)

func TestParseWatchSpec(t *testing.T) {
	// The bare no-argument case is the one the whole design is built around,
	// so it is tested against a real directory rather than a stub.
	t.Run("no arguments follows the working directory", func(t *testing.T) {
		dir := t.TempDir()
		got, err := ParseWatchSpec(nil, dir)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != WatchTree {
			t.Errorf("Kind = %q, want %q", got.Kind, WatchTree)
		}
		abs, _ := filepath.Abs(dir)
		if got.Path != abs {
			t.Errorf("Path = %q, want the absolute %q", got.Path, abs)
		}
		if got.ID != TreeWatchID(abs) {
			t.Errorf("ID = %q, want %q", got.ID, TreeWatchID(abs))
		}
	})

	tests := []struct {
		name string
		args []string
		// setup supplies the arguments (and the ID they should produce) for the
		// cases that need something on disk: a directory is only a watch target
		// when it exists, so those cases cannot be written as literals.
		setup    func(t *testing.T) (args []string, wantID string)
		resolve  func([]string) (target.Target, error)
		wantID   string
		wantKind WatchKind
		wantErr  string
	}{
		{
			// The form the monitor itself prints, so it has to round-trip.
			name:     "owner/repo#42 shorthand",
			args:     []string{"justanotherspy/shuck#42"},
			wantID:   "pr:justanotherspy/shuck#42",
			wantKind: WatchPR,
		},
		{
			name: "explicit owner/repo and number",
			args: []string{"justanotherspy/shuck", "42"},
			resolve: func([]string) (target.Target, error) {
				return target.Target{Owner: "justanotherspy", Repo: "shuck", Number: 42}, nil
			},
			wantID:   "pr:justanotherspy/shuck#42",
			wantKind: WatchPR,
		},
		{
			name: "PR URL",
			args: []string{"https://github.com/justanotherspy/shuck/pull/42"},
			resolve: func([]string) (target.Target, error) {
				return target.Target{Owner: "justanotherspy", Repo: "shuck", Number: 42}, nil
			},
			wantID:   "pr:justanotherspy/shuck#42",
			wantKind: WatchPR,
		},
		{
			// A run URL resolves but names no PR, which the monitor cannot
			// follow — it watches pull requests, not one-off runs.
			name:    "a target with no PR number is rejected",
			args:    []string{"https://github.com/o/r/actions/runs/1"},
			resolve: func([]string) (target.Target, error) { return target.Target{Owner: "o", Repo: "r", RunID: 1}, nil },
			wantErr: "does not name a pull request",
		},
		{
			name:    "resolution failure is reported",
			args:    []string{"nonsense"},
			resolve: func([]string) (target.Target, error) { return target.Target{}, errors.New("invalid PR number") },
			wantErr: "invalid PR number",
		},
		{
			name:    "hash shorthand with a bad number falls through to resolution",
			args:    []string{"o/r#zero"},
			resolve: func([]string) (target.Target, error) { return target.Target{}, errors.New("invalid PR number") },
			wantErr: "invalid PR number",
		},
		{
			// The documented directory target: `shuck monitor watch ~/src/repo`
			// follows that tree, exactly as the no-argument form follows this one.
			name: "a directory becomes a tree watch",
			setup: func(t *testing.T) ([]string, string) {
				dir := t.TempDir()
				return []string{dir}, TreeWatchID(dir)
			},
			resolve:  func([]string) (target.Target, error) { return target.Target{}, errors.New("invalid PR number") },
			wantKind: WatchTree,
		},
		{
			// The watch outlives the shell that named it, so a relative path
			// must be stored absolute — the daemon has its own working
			// directory and would otherwise read the wrong tree, or none.
			name: "a relative directory is stored absolute",
			setup: func(t *testing.T) ([]string, string) {
				root := t.TempDir()
				if err := os.Mkdir(filepath.Join(root, "tree"), 0o750); err != nil {
					t.Fatal(err)
				}
				t.Chdir(root)
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				return []string{"tree"}, TreeWatchID(filepath.Join(cwd, "tree"))
			},
			resolve:  func([]string) (target.Target, error) { return target.Target{}, errors.New("invalid PR number") },
			wantKind: WatchTree,
		},
		{
			// The ordering guard: PR spellings are tried first, so a directory
			// that happens to be named "42" cannot quietly shadow PR #42.
			name: "a directory named like a PR number is still the PR",
			setup: func(t *testing.T) ([]string, string) {
				root := t.TempDir()
				if err := os.Mkdir(filepath.Join(root, "42"), 0o750); err != nil {
					t.Fatal(err)
				}
				t.Chdir(root)
				return []string{"42"}, "pr:o/r#42"
			},
			resolve:  func([]string) (target.Target, error) { return target.Target{Owner: "o", Repo: "r", Number: 42}, nil },
			wantKind: WatchPR,
		},
		{
			// The fallback must not swallow the parse error: a path with a typo
			// in it names nothing, and saying so is the only useful answer.
			name:    "a path that is not there stays a PR-spec error",
			args:    []string{"./no-such-tree"},
			resolve: func([]string) (target.Target, error) { return target.Target{}, errors.New("invalid PR number") },
			wantErr: "invalid PR number",
		},
		{
			name: "a file is not a working tree",
			setup: func(t *testing.T) ([]string, string) {
				f := filepath.Join(t.TempDir(), "notatree")
				if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				return []string{f}, ""
			},
			resolve: func([]string) (target.Target, error) { return target.Target{}, errors.New("invalid PR number") },
			wantErr: "invalid PR number",
		},
		{
			// Two arguments are the "owner/repo 42" pair; neither half is a
			// directory, so a bad number there is a plain error.
			name:    "a two-argument spec never falls back to a directory",
			args:    []string{"o/r", "notanumber"},
			resolve: func([]string) (target.Target, error) { return target.Target{}, errors.New("invalid PR number") },
			wantErr: "invalid PR number",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.resolve != nil {
				original := resolveTarget
				resolveTarget = tt.resolve
				t.Cleanup(func() { resolveTarget = original })
			}
			args, wantID := tt.args, tt.wantID
			if tt.setup != nil {
				args, wantID = tt.setup(t)
			}

			got, err := ParseWatchSpec(args, "")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWatchSpec: %v", err)
			}
			if got.ID != wantID {
				t.Errorf("ID = %q, want %q", got.ID, wantID)
			}
			if got.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tt.wantKind)
			}
			if tt.wantKind == WatchTree && got.Path == "" {
				t.Error("a tree watch must carry the path it follows")
			}
		})
	}
}

func TestSplitHashSpec(t *testing.T) {
	tests := []struct {
		in     string
		wantOK bool
	}{
		{"o/r#42", true},
		{"o/r", false},         // no number
		{"o/r#0", false},       // not a PR number
		{"o/r#-1", false},      // not a PR number
		{"o#42", false},        // no repo
		{"/r#42", false},       // no owner
		{"a/b/c#42", false},    // not an owner/repo slug
		{"o/r#notanum", false}, // not a number
	}
	for _, tt := range tests {
		_, _, _, ok := splitHashSpec(tt.in)
		if ok != tt.wantOK {
			t.Errorf("splitHashSpec(%q) ok = %v, want %v", tt.in, ok, tt.wantOK)
		}
	}
}

func TestWatchTargetAndDescribe(t *testing.T) {
	tests := []struct {
		name         string
		watch        Watch
		wantTarget   string
		wantDescribe []string
	}{
		{
			name:         "resolved tree watch",
			watch:        Watch{Kind: WatchTree, Path: "/w", Owner: "o", Repo: "r", Number: 7, Branch: "feature"},
			wantTarget:   "o/r#7",
			wantDescribe: []string{"/w", "o/r#7", "feature"},
		},
		{
			name:         "tree watch with a repo but no PR",
			watch:        Watch{Kind: WatchTree, Path: "/w", Owner: "o", Repo: "r", Branch: "main", Note: "no open PR for main"},
			wantTarget:   "o/r",
			wantDescribe: []string{"/w", "o/r on main", "no open PR"},
		},
		{
			name:         "unresolved tree watch",
			watch:        Watch{Kind: WatchTree, Path: "/w"},
			wantTarget:   "",
			wantDescribe: []string{"/w", "not resolved yet"},
		},
		{
			name:         "pinned PR watch",
			watch:        Watch{Kind: WatchPR, Owner: "o", Repo: "r", Number: 7},
			wantTarget:   "o/r#7",
			wantDescribe: []string{"o/r#7"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.watch.Target(); got != tt.wantTarget {
				t.Errorf("Target() = %q, want %q", got, tt.wantTarget)
			}
			got := tt.watch.Describe()
			for _, want := range tt.wantDescribe {
				if !strings.Contains(got, want) {
					t.Errorf("Describe() = %q, want it to contain %q", got, want)
				}
			}
			if strings.Contains(got, "  ") {
				t.Errorf("Describe() = %q, want no doubled spaces", got)
			}
		})
	}
}

func TestRegistry(t *testing.T) {
	p := newPaths(t.TempDir())
	r := loadRegistry(p)

	if r.Len() != 0 {
		t.Fatalf("a fresh registry has %d watches, want 0", r.Len())
	}

	first := r.Add(Watch{ID: "tree:/a", Kind: WatchTree, Path: "/a"})
	r.Add(Watch{ID: "pr:o/r#1", Kind: WatchPR, Owner: "o", Repo: "r", Number: 1})
	if r.Len() != 2 {
		t.Fatalf("Len = %d, want 2", r.Len())
	}
	if first.Added.IsZero() || first.LastSeen.IsZero() {
		t.Error("a new watch should be stamped")
	}

	// Re-adding must not discard what the poller has already resolved.
	first.Owner, first.Repo, first.Number = "o", "r", 9
	again := r.Add(Watch{ID: "tree:/a", Kind: WatchTree, Path: "/a"})
	if again.Number != 9 {
		t.Errorf("re-adding reset the resolved PR (Number = %d, want 9)", again.Number)
	}

	list := r.List()
	if len(list) != 2 || list[0].ID != "pr:o/r#1" {
		t.Errorf("List() is not sorted by ID: %v", list)
	}

	if _, ok := r.Get("tree:/a"); !ok {
		t.Error("Get should find a stored watch")
	}
	if !r.Remove("tree:/a") {
		t.Error("Remove should report removing a watch")
	}
	if r.Remove("tree:/a") {
		t.Error("Remove should report a second removal as a no-op")
	}

	// The set is persisted, so a restart resumes following the same things.
	reloaded := loadRegistry(p)
	if reloaded.Len() != 1 {
		t.Errorf("reloaded registry has %d watches, want 1", reloaded.Len())
	}
}

func TestRegistryTouchAndExpire(t *testing.T) {
	r := loadRegistry(newPaths(t.TempDir()))
	r.Add(Watch{ID: "a", Kind: WatchTree, Path: "/a"})
	r.Add(Watch{ID: "b", Kind: WatchTree, Path: "/b"})

	// Age one watch past the TTL and leave the other as a live session would:
	// a status/events/poke call runs TouchAll, so a watch is only stale when
	// nobody has asked the monitor how things stand since.
	stale := time.Now().Add(-2 * time.Hour)
	b, _ := r.Get("b")
	b.LastSeen = stale

	dropped := r.Expire(time.Hour, time.Now())
	if len(dropped) != 1 || dropped[0].ID != "b" {
		t.Fatalf("expired %v, want just the untouched watch b", dropped)
	}
	if r.Len() != 1 {
		t.Errorf("Len = %d after expiry, want 1", r.Len())
	}

	// A negative or zero TTL means never expire.
	w, _ := r.Get("a")
	w.LastSeen = stale
	if got := r.Expire(0, time.Now()); got != nil {
		t.Errorf("a zero TTL expired %v, want nothing", got)
	}

	r.TouchAll()
	if w, _ := r.Get("a"); time.Since(w.LastSeen) > time.Minute {
		t.Error("TouchAll should refresh every watch")
	}
}

func TestRegistryIgnoresUnreadableState(t *testing.T) {
	p := newPaths(t.TempDir())
	if err := writeFileAtomic(p.watches, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	// Losing the watch list costs a session one `monitor watch` call; refusing
	// to start would cost it the monitor.
	if r := loadRegistry(p); r.Len() != 0 {
		t.Errorf("Len = %d, want an empty registry", r.Len())
	}
}

// TestParseWatchSpecScopesToTheRegisteringSession pins the half of scoping that
// happens on the client: a watch has to carry the working tree of whoever asked
// for it, or the daemon has nothing to filter the feed by and every session
// sees every other session's CI.
func TestParseWatchSpecScopesToTheRegisteringSession(t *testing.T) {
	tree := t.TempDir()
	other := t.TempDir()

	tests := []struct {
		name string
		args []string
		cwd  string
		// want is the set of scopes, as absolute paths resolved from the
		// fixtures above.
		want []string
	}{
		{
			name: "the working tree scopes itself",
			cwd:  tree,
			want: []string{tree},
		},
		{
			// The case that makes "sources added by agents explicitly" belong
			// to the agent that added them: the PR names no directory, so the
			// only thing that can scope it is where the command was run.
			name: "an explicit PR belongs to the tree it was added from",
			args: []string{"o/r#42"},
			cwd:  tree,
			want: []string{tree},
		},
		{
			// Both, because either could be the session that wants the news:
			// the tree being followed, and the tree the person was standing in
			// when they said so.
			name: "a directory argument scopes to itself and to the caller",
			args: []string{other},
			cwd:  tree,
			want: []string{other, tree},
		},
		{
			name: "a caller with no directory leaves the watch unscoped",
			args: []string{"o/r#42"},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseWatchSpec(tt.args, tt.cwd)
			if err != nil {
				t.Fatalf("ParseWatchSpec: %v", err)
			}
			want := make([]string, 0, len(tt.want))
			for _, p := range tt.want {
				abs, _ := filepath.Abs(p)
				want = append(want, abs)
			}
			if len(got.Scopes) != len(want) {
				t.Fatalf("Scopes = %v, want %v", got.Scopes, want)
			}
			for i := range want {
				if got.Scopes[i] != want[i] {
					t.Errorf("Scopes[%d] = %q, want %q", i, got.Scopes[i], want[i])
				}
			}
		})
	}
}

// TestParseWatchSpecScopesAPRToTheCheckoutRoot covers the completely ordinary
// thing an agent does: `cd internal/cli && shuck monitor watch owner/repo#42`.
// Scopes are matched exactly, and the only directory a session ever asks with
// is the one it opened in — so a watch scoped to the subdirectory alone is
// claimed by nobody and its CI failures reach no session at all.
func TestParseWatchSpecScopesAPRToTheCheckoutRoot(t *testing.T) {
	root := t.TempDir()
	writeRepo(t, root, "ref: refs/heads/main\n", "refs/heads/main", "abc\n", originConfig)
	sub := filepath.Join(root, "internal", "cli")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	loose := t.TempDir() // no repository above it at all

	tests := []struct {
		name string
		cwd  string
		want []string
		// asks are the directories a reader may later filter with; each must
		// find the watch in scope.
		asks []string
	}{
		{
			name: "a subdirectory of the checkout scopes the checkout too",
			cwd:  sub,
			want: []string{sub, root},
			asks: []string{sub, root},
		},
		{
			name: "the checkout root is already the root",
			cwd:  root,
			want: []string{root},
			asks: []string{root},
		},
		{
			name: "a directory in no repository has only itself to offer",
			cwd:  loose,
			want: []string{loose},
			asks: []string{loose},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := ParseWatchSpec([]string{"o/r#42"}, tt.cwd)
			if err != nil {
				t.Fatalf("ParseWatchSpec: %v", err)
			}
			if !slices.Equal(w.Scopes, tt.want) {
				t.Fatalf("Scopes = %v, want %v", w.Scopes, tt.want)
			}
			for _, ask := range tt.asks {
				if !w.InScope(ask) {
					t.Errorf("InScope(%q) = false; the PR this session asked to follow would report to nobody", ask)
				}
			}
		})
	}
}

// TestParseWatchSpecKeepsTheWatchIdentity guards the one thing scoping must not
// change: a watch is still keyed by what it names, so the same session
// re-registering the same tree updates its watch rather than duplicating it.
func TestParseWatchSpecKeepsTheWatchIdentity(t *testing.T) {
	tree := t.TempDir()
	abs, _ := filepath.Abs(tree)

	first, err := ParseWatchSpec(nil, tree)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParseWatchSpec([]string{tree}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != TreeWatchID(abs) || second.ID != first.ID {
		t.Errorf("ids = %q and %q, want both %q", first.ID, second.ID, TreeWatchID(abs))
	}
}

func TestWatchInScope(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "repo")
	// The layout that makes containment matching wrong: this repository keeps
	// its git worktrees inside the main checkout.
	worktree := filepath.Join(tree, ".claude", "worktrees", "feature")
	for _, dir := range []string{tree, worktree} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name   string
		scopes []string
		ask    string
		want   bool
	}{
		{
			// The compatibility direction, and the one that must never drop an
			// event: a watch a daemon of an older build persisted has no scope
			// at all, and belongs to everyone until a registration teaches it
			// otherwise.
			name: "a watch with no scopes belongs to every session",
			ask:  tree,
			want: true,
		},
		{
			name:   "its own tree",
			scopes: []string{tree},
			ask:    tree,
			want:   true,
		},
		{
			name:   "one of several",
			scopes: []string{worktree, tree},
			ask:    tree,
			want:   true,
		},
		{
			name:   "an unrelated tree",
			scopes: []string{worktree},
			ask:    tree,
			want:   false,
		},
		{
			// Containment would say yes here, and saying yes is the observed
			// bug: a session in the main checkout was handed the worktree's CI.
			name:   "a worktree nested inside the checkout is not the checkout",
			scopes: []string{tree},
			ask:    worktree,
			want:   false,
		},
		{
			name:   "an unresolvable question matches nothing scoped",
			scopes: []string{tree},
			ask:    "",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := Watch{ID: "w", Scopes: tt.scopes}
			if got := w.InScope(tt.ask); got != tt.want {
				t.Errorf("InScope(%q) = %v, want %v", tt.ask, got, tt.want)
			}
		})
	}
}

// TestRegistryAddMergesScopes covers the upgrade and the second-session paths
// at once. Add keeps the stored watch and discards the incoming one, which is
// right for everything the poller has resolved and wrong for the registrant:
// without the merge, a watch first seen without a scope — or first registered
// by another tree — would filter its own session out of the feed forever.
func TestRegistryAddMergesScopes(t *testing.T) {
	p := newPaths(t.TempDir())
	r := loadRegistry(p)

	// A watch as a daemon that predates scoping left it: no scope at all.
	r.Add(Watch{ID: "pr:o/r#42", Kind: WatchPR, Owner: "o", Repo: "r", Number: 42})
	r.Add(Watch{ID: "pr:o/r#42", Kind: WatchPR, Owner: "o", Repo: "r", Number: 42, Scopes: []string{"/one"}})
	r.Add(Watch{ID: "pr:o/r#42", Kind: WatchPR, Owner: "o", Repo: "r", Number: 42, Scopes: []string{"/two"}})
	// The same session again: a scope it already has is not a new one.
	r.Add(Watch{ID: "pr:o/r#42", Kind: WatchPR, Owner: "o", Repo: "r", Number: 42, Scopes: []string{"/one"}})

	w, ok := r.Get("pr:o/r#42")
	if !ok {
		t.Fatal("the watch disappeared")
	}
	// Both registrants, once each — and the one that registered again is the
	// most recent, so it is the last thing the cap would displace.
	if len(w.Scopes) != 2 || w.Scopes[0] != "/two" || w.Scopes[1] != "/one" {
		t.Fatalf("Scopes = %v, want both registrants once each, least recent first", w.Scopes)
	}

	// And it survives the daemon, or a restart would leave both sessions
	// unfiltered again.
	reloaded, ok := loadRegistry(p).Get("pr:o/r#42")
	if !ok {
		t.Fatal("the watch did not survive a reload")
	}
	if len(reloaded.Scopes) != 2 {
		t.Errorf("Scopes = %v after a reload, want both", reloaded.Scopes)
	}
}

// TestAddScopeIsBounded keeps a persisted set from growing without limit: the
// id is stable, so a PR watch would otherwise accumulate an entry for every
// directory anyone ever registered it from.
func TestAddScopeIsBounded(t *testing.T) {
	tests := []struct {
		name string
		// add is the sequence of registrations, in order.
		add []string
		// keep and drop are what must and must not survive the cap.
		keep []string
		drop []string
	}{
		{
			name: "the oldest registrant is what a new one displaces",
			add:  scopeSeq(0, maxWatchScopes*2),
			keep: []string{"/tree/" + strconv.Itoa(maxWatchScopes*2-1)},
			drop: []string{"/tree/0"},
		},
		{
			// The finding: nothing re-registers a PR watch on its own, so a
			// session that keeps asking for one is the only evidence it is
			// still there. Evicting in arrival order would unsubscribe it from
			// a PR it explicitly asked to follow, with nothing in the status
			// output to say so.
			name: "a session that registers again is not the oldest any more",
			add:  append(append([]string{"/keeps/asking"}, scopeSeq(0, maxWatchScopes-1)...), "/keeps/asking", "/tree/new"),
			keep: []string{"/keeps/asking", "/tree/new"},
			drop: []string{"/tree/0"},
		},
		{
			name: "registering the same scope twice does not spend two slots",
			add:  []string{"/a", "/a", "/b", "/b"},
			keep: []string{"/a", "/b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var w Watch
			for _, scope := range tt.add {
				w.addScope(scope)
			}
			if len(w.Scopes) > maxWatchScopes {
				t.Fatalf("kept %d scopes, want the set capped at %d", len(w.Scopes), maxWatchScopes)
			}
			for _, scope := range tt.keep {
				if !slices.Contains(w.Scopes, scope) {
					t.Errorf("Scopes = %v, want %q kept — that session is filtered out of its own feed without it", w.Scopes, scope)
				}
			}
			for _, scope := range tt.drop {
				if slices.Contains(w.Scopes, scope) {
					t.Errorf("Scopes = %v, want %q displaced by a more recent registrant", w.Scopes, scope)
				}
			}
		})
	}
}

// scopeSeq builds a run of distinct registrant directories.
func scopeSeq(from, to int) []string {
	out := make([]string, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, "/tree/"+strconv.Itoa(i))
	}
	return out
}
