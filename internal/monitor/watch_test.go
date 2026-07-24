package monitor

import (
	"errors"
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
		name     string
		args     []string
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.resolve != nil {
				original := resolveTarget
				resolveTarget = tt.resolve
				t.Cleanup(func() { resolveTarget = original })
			}

			got, err := ParseWatchSpec(tt.args, "")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWatchSpec: %v", err)
			}
			if got.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tt.wantID)
			}
			if got.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tt.wantKind)
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

	// Age both watches past the TTL, then keep one alive by asking about it —
	// which is exactly what a live session does.
	stale := time.Now().Add(-2 * time.Hour)
	for _, id := range []string{"a", "b"} {
		w, _ := r.Get(id)
		w.LastSeen = stale
	}
	r.Touch("a")

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
	// Touching an unknown ID is a no-op, not a panic.
	r.Touch("nope")
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
