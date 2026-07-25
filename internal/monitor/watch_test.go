package monitor

import (
	"errors"
	"os"
	"path/filepath"
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
