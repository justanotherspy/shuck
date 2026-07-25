package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())

	want := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 7, HeadSHA: "abc1234", Title: "fix"},
		FailedJobs: []model.JobResult{{
			ID: 1, RunAttempt: 1, Name: "build", Conclusion: "failure", Inspected: true,
			FailedSteps: []model.FailedStep{{Number: 3, Name: "test", Command: "go test ./...", Kind: model.KindBash, Excerpt: "FAIL"}},
		}},
	}
	if err := Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load("o", "r", 7)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil {
		t.Fatal("Load returned nil for a saved report")
	}
	if got.PR.HeadSHA != "abc1234" || len(got.FailedJobs) != 1 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.FailedJobs[0].FailedSteps[0].Command != "go test ./..." {
		t.Errorf("step command not preserved: %+v", got.FailedJobs[0].FailedSteps[0])
	}
}

func TestLoadMissingReturnsNil(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	got, err := Load("o", "r", 999)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing cache, got %+v", got)
	}
}

// TestBaseDefaultsToUserCacheDir verifies that, absent SHUCK_HOME, the base
// follows the XDG cache directory (~/.cache/shuck) via os.UserCacheDir rather
// than the legacy ~/.shuck home location. XDG_CACHE_HOME steers UserCacheDir on
// the test platform, so it doubles as a check that the env var is honored.
func TestBaseDefaultsToUserCacheDir(t *testing.T) {
	t.Setenv("SHUCK_HOME", "")
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)

	got, err := Base()
	if err != nil {
		t.Fatalf("Base: %v", err)
	}
	if want := filepath.Join(cacheRoot, "shuck"); got != want {
		t.Errorf("Base() = %q, want %q", got, want)
	}
}

// TestBaseHonorsShuckHome confirms SHUCK_HOME still overrides the default base
// outright, keeping the test/override escape hatch intact.
func TestBaseHonorsShuckHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SHUCK_HOME", dir)
	got, err := Base()
	if err != nil {
		t.Fatalf("Base: %v", err)
	}
	if got != dir {
		t.Errorf("Base() = %q, want %q", got, dir)
	}
}

func TestSaveLoadJobLogRoundTrip(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())

	const raw = "##[group]Run go test\nFAIL\n##[error]boom\n"
	if err := SaveJobLog("o", "r", 7, 42, 1, raw); err != nil {
		t.Fatalf("SaveJobLog: %v", err)
	}
	got, ok, err := LoadJobLog("o", "r", 7, 42, 1)
	if err != nil {
		t.Fatalf("LoadJobLog: %v", err)
	}
	if !ok || got != raw {
		t.Fatalf("LoadJobLog ok=%v got=%q want %q", ok, got, raw)
	}

	if _, ok, err := LoadJobLog("o", "r", 7, 42, 2); err != nil || ok {
		t.Fatalf("missing attempt: ok=%v err=%v", ok, err)
	}
}

// TestJobLogPathAgreesWithSaveAndLoad pins the one thing JobLogPath exists for:
// the path it hands an agent is exactly where SaveJobLog writes and LoadJobLog
// reads. The monitor calls JobLogPath first and puts the result in a CI-failure
// event, so if the two ever disagree the event points at a file that is never
// there. It is also a pure query — asking where a log *would* live must not
// create the directory that would make a later "does it exist?" check lie.
func TestJobLogPathAgreesWithSaveAndLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHUCK_HOME", home)

	const raw = "##[group]Run go test\nFAIL\n##[error]boom\n"
	path, err := JobLogPath("o", "r", 7, 42, 3)
	if err != nil {
		t.Fatalf("JobLogPath: %v", err)
	}

	// Nothing on disk yet, and nothing created by asking.
	if entries, rerr := os.ReadDir(home); rerr != nil || len(entries) != 0 {
		t.Fatalf("JobLogPath created %d entries under the cache base (err=%v), want none", len(entries), rerr)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatalf("JobLogPath reported an existing file before any save: stat err=%v", serr)
	}

	if err := SaveJobLog("o", "r", 7, 42, 3, raw); err != nil {
		t.Fatalf("SaveJobLog: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("SaveJobLog did not write to the path JobLogPath reported (%s): %v", path, err)
	}
	if string(got) != raw {
		t.Errorf("file at JobLogPath = %q, want %q", got, raw)
	}
	loaded, ok, err := LoadJobLog("o", "r", 7, 42, 3)
	if err != nil || !ok {
		t.Fatalf("LoadJobLog after SaveJobLog: ok=%v err=%v", ok, err)
	}
	if loaded != string(got) {
		t.Errorf("LoadJobLog read %q, the file at JobLogPath holds %q", loaded, got)
	}

	// The layout itself is documented and agent-facing (the event tells a human
	// or an agent to open this file), so the shape is part of the contract: a
	// swapped jobID/attempt would still round-trip through Save/Load unnoticed.
	// Spelled out literally against the temp SHUCK_HOME rather than derived from
	// Dir — an oracle built from the production path builder moves with it, so
	// it would accept any relayout of the cache tree.
	if want := filepath.Join(home, "cache", "o", "r", "7", "logs", "42-3.log"); path != want {
		t.Errorf("JobLogPath = %q, want %q", path, want)
	}
}

// TestJobLogPathDistinctPerTarget guards the naming: every
// owner/repo/pr/job/attempt tuple gets its own file. Each case carries the
// literal path it must produce, relative to the temp SHUCK_HOME — an oracle
// that shares nothing with the production join, so dropping a component,
// reordering owner and repo, or renaming a level of the tree all show up here.
// Several rows exist only to collide under a specific slip: 1/23 vs 12/3 the
// moment the "-" leaves the file name, a/bc vs ab/c the moment owner and repo
// stop being separate directories, and x/y vs y/x the moment their order stops
// mattering. A round-trip test notices none of those, because Save and Load
// would collide identically.
func TestJobLogPathDistinctPerTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHUCK_HOME", home)

	cases := []struct {
		owner, repo string
		pr          int
		jobID       int64
		attempt     int
		want        string // slash-separated, relative to SHUCK_HOME
	}{
		{"o", "r", 7, 42, 1, "cache/o/r/7/logs/42-1.log"},
		{"o", "r", 7, 42, 2, "cache/o/r/7/logs/42-2.log"},
		{"o", "r", 7, 43, 1, "cache/o/r/7/logs/43-1.log"},
		{"o", "r", 7, 1, 23, "cache/o/r/7/logs/1-23.log"},
		{"o", "r", 7, 12, 3, "cache/o/r/7/logs/12-3.log"},
		{"o", "r", 8, 42, 1, "cache/o/r/8/logs/42-1.log"},
		{"o", "r2", 7, 42, 1, "cache/o/r2/7/logs/42-1.log"},
		{"o2", "r", 7, 42, 1, "cache/o2/r/7/logs/42-1.log"},
		{"a", "bc", 7, 42, 1, "cache/a/bc/7/logs/42-1.log"},
		{"ab", "c", 7, 42, 1, "cache/ab/c/7/logs/42-1.log"},
		{"x", "y", 7, 42, 1, "cache/x/y/7/logs/42-1.log"},
		{"y", "x", 7, 42, 1, "cache/y/x/7/logs/42-1.log"},
		{"octo-cat", "my.repo_v2", 1, 1, 1, "cache/octo-cat/my.repo_v2/1/logs/1-1.log"},
		{"OCTO", "github.io", 0, 0, 0, "cache/OCTO/github.io/0/logs/0-0.log"},
	}

	seen := make(map[string]string, len(cases))
	for _, c := range cases {
		label := fmt.Sprintf("(%s,%s,%d,%d,%d)", c.owner, c.repo, c.pr, c.jobID, c.attempt)
		path, err := JobLogPath(c.owner, c.repo, c.pr, c.jobID, c.attempt)
		if err != nil {
			t.Fatalf("JobLogPath%s: %v", label, err)
		}
		if want := filepath.Join(home, filepath.FromSlash(c.want)); path != want {
			t.Errorf("JobLogPath%s = %q, want %q", label, path, want)
		}
		if prev, dup := seen[path]; dup {
			t.Fatalf("JobLogPath%s collides with %s at %s", label, prev, path)
		}
		seen[path] = label
	}

	// And the separation is real on disk, not just in the strings.
	body := func(c int) string { return fmt.Sprintf("case %d", c) }
	for i, c := range cases {
		if err := SaveJobLog(c.owner, c.repo, c.pr, c.jobID, c.attempt, body(i)); err != nil {
			t.Fatalf("SaveJobLog(%s,%s,%d,%d,%d): %v", c.owner, c.repo, c.pr, c.jobID, c.attempt, err)
		}
	}
	for i, c := range cases {
		got, ok, err := LoadJobLog(c.owner, c.repo, c.pr, c.jobID, c.attempt)
		if err != nil || !ok {
			t.Fatalf("LoadJobLog(%s,%s,%d,%d,%d): ok=%v err=%v", c.owner, c.repo, c.pr, c.jobID, c.attempt, ok, err)
		}
		if got != body(i) {
			t.Errorf("LoadJobLog(%s,%s,%d,%d,%d) = %q, want %q — logs overwrote each other",
				c.owner, c.repo, c.pr, c.jobID, c.attempt, got, body(i))
		}
	}
}

// TestJobLogPathStaysUnderCacheBase is the security contract. owner/repo reach
// here from CLI arguments, GitHub URLs, and git remotes, and the path is handed
// to an agent that may read it, so a crafted segment must never name a file
// outside ~/.cache/shuck. Traversal inputs are rejected outright (no path at
// all); anything that *is* accepted has to stay inside the base.
func TestJobLogPathStaysUnderCacheBase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHUCK_HOME", home)

	hostile := []struct{ owner, repo string }{
		{"..", "r"},
		{"o", ".."},
		{".", "r"},
		{"o", "."},
		{"a/../../b", "r"},
		{"o", "a/../../b"},
		{"../../../../etc", "passwd"},
		{"o", "../../../../tmp/evil"},
		{"o/x", "r"},
		{"o", `..\..\win`},
		{`..\..\win`, "r"},
		{"", "r"},
		{"o", ""},
		{"o\x00", "r"},
		{"o", "r\x00/../.."},
		// Embedded "..": no separator, no NUL, and not the bare "." / ".."
		// spellings, so safeSegment's `strings.Contains(s, "..")` clause is the
		// only thing rejecting these. Its rule is deliberately broader than
		// "is this segment literally ..": no accepted segment may carry ".."
		// anywhere, because a real owner/repo never does and the value is
		// re-joined, printed, and re-parsed downstream. Without cases only that
		// clause catches, it can be deleted outright with the suite still
		// green, leaving traversal defense resting on filepath.Join's cleaning
		// rather than on rejecting the input.
		{"a..b", "r"},
		{"o", "a..b"},
		{"..evil", "r"},
		{"o", "evil.."},
		{"...", "r"},
		{"o", "..."},
	}
	for _, c := range hostile {
		path, err := JobLogPath(c.owner, c.repo, 7, 42, 1)
		if err == nil {
			t.Errorf("JobLogPath(%q,%q) accepted a traversal segment, path=%q", c.owner, c.repo, path)
		}
		if path != "" {
			t.Errorf("JobLogPath(%q,%q) returned path %q alongside err=%v; a rejected target must name no file",
				c.owner, c.repo, path, err)
		}
	}

	// Legitimate names — including the dots and dashes real repos carry — are
	// accepted, and land at exactly this spot inside the base. Asserting the
	// literal path rather than mere containment matters: once safeSegment has
	// rejected every separator, "the result is under the base" is true by
	// construction of filepath.Join and holds however wrong the layout is.
	legit := []struct{ owner, repo, want string }{
		{"o", "r", "cache/o/r/7/logs/42-1.log"},
		{"octo-cat", "my.repo_v2", "cache/octo-cat/my.repo_v2/7/logs/42-1.log"},
		{"OCTO", "github.io", "cache/OCTO/github.io/7/logs/42-1.log"},
		{"-leading-dash", "trailing.dot.", "cache/-leading-dash/trailing.dot./7/logs/42-1.log"},
	}
	for _, c := range legit {
		path, err := JobLogPath(c.owner, c.repo, 7, 42, 1)
		if err != nil {
			t.Fatalf("JobLogPath(%q,%q) rejected a legitimate name: %v", c.owner, c.repo, err)
		}
		if want := filepath.Join(home, filepath.FromSlash(c.want)); path != want {
			t.Errorf("JobLogPath(%q,%q) = %q, want %q", c.owner, c.repo, path, want)
		}
	}
}

// TestJobLogPathSurfacesBaseError covers the one failure Base itself can
// report: with SHUCK_HOME unset and no home directory to fall back on,
// os.UserCacheDir fails. JobLogPath must pass that up rather than swallow it —
// a dropped error check would leave base "" and hand back a relative path like
// "cache/o/r/7/logs/42-1.log", which the monitor would publish as a log
// location and SaveJobLog would write into the process's working directory.
func TestJobLogPathSurfacesBaseError(t *testing.T) {
	t.Setenv("SHUCK_HOME", "")
	// Every variable os.UserCacheDir consults, across platforms.
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("LocalAppData", "")
	t.Setenv("home", "") // plan9
	// If the error ever is swallowed, the relative cache tree that SaveJobLog
	// then creates lands here rather than in the package directory.
	t.Chdir(t.TempDir())

	if _, err := Base(); err == nil {
		t.Fatal("Base() err=nil with SHUCK_HOME and every cache-dir variable cleared; the rest of this test needs a failing Base")
	}

	path, err := JobLogPath("o", "r", 7, 42, 1)
	if err == nil {
		t.Errorf("JobLogPath with an unresolvable cache base err=nil, path=%q", path)
	}
	if path != "" {
		t.Errorf("JobLogPath returned path %q alongside err=%v; a target with no base names no file", path, err)
	}
	// The same error has to reach the callers that build on it, or Save/Load
	// would fall back to a relative cache tree instead of failing.
	if _, err := Dir("o", "r", 7); err == nil {
		t.Error("Dir with an unresolvable cache base err=nil, want error")
	}
	if err := SaveJobLog("o", "r", 7, 42, 1, "log"); err == nil {
		t.Error("SaveJobLog with an unresolvable cache base err=nil, want error")
	}
	if _, _, err := LoadJobLog("o", "r", 7, 42, 1); err == nil {
		t.Error("LoadJobLog with an unresolvable cache base err=nil, want error")
	}
}

func TestPurge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHUCK_HOME", home)

	// A stale PR entry (with a cached log) and a fresh one we keep.
	if err := Save(&model.Report{PR: model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "old"}}); err != nil {
		t.Fatalf("Save stale: %v", err)
	}
	if err := SaveJobLog("o", "r", 1, 9, 1, "log"); err != nil {
		t.Fatalf("SaveJobLog: %v", err)
	}
	if err := Save(&model.Report{PR: model.PR{Owner: "o", Repo: "r", Number: 2, HeadSHA: "new"}}); err != nil {
		t.Fatalf("Save fresh: %v", err)
	}

	staleDir, _ := Dir("o", "r", 1)
	keepDir, _ := Dir("o", "r", 2)
	// Backdate the stale entry's record file beyond the TTL.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(staleDir, fileName), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := Purge(time.Hour, keepDir); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Errorf("stale entry should be purged, stat err=%v", err)
	}
	if _, err := os.Stat(keepDir); err != nil {
		t.Errorf("fresh entry should survive, stat err=%v", err)
	}
}

func TestPurgeSkipsKeptStaleEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHUCK_HOME", home)
	if err := Save(&model.Report{PR: model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "x"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	dir, _ := Dir("o", "r", 1)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, fileName), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := Purge(time.Hour, dir); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("kept entry should survive even when stale, stat err=%v", err)
	}
}

func TestDirRejectsPathTraversal(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	cases := []struct{ owner, repo string }{
		{"o", "../../../../tmp/evil"},
		{"..", "r"},
		{"o", ".."},
		{"o/x", "r"},
		{"o", "r/../../etc"},
		{"", "r"},
		{"o", ""},
		{`o`, `..\..\win`},
		// Embedded "..", separator-free: only safeSegment's Contains(s, "..")
		// clause rejects these, so they are what keeps that clause honest.
		{"a..b", "r"},
		{"o", "a..b"},
		{"..evil", "r"},
		{"o", "..."},
	}
	for _, c := range cases {
		if _, err := Dir(c.owner, c.repo, 1); err == nil {
			t.Errorf("Dir(%q,%q) accepted a traversal segment", c.owner, c.repo)
		}
		if _, err := ActionDir(c.owner, c.repo); err == nil {
			t.Errorf("ActionDir(%q,%q) accepted a traversal segment", c.owner, c.repo)
		}
		if _, err := SecurityDir(c.owner, c.repo); err == nil {
			t.Errorf("SecurityDir(%q,%q) accepted a traversal segment", c.owner, c.repo)
		}
	}
}

func TestDirAllowsLegitimateNames(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	// A dot is legal in a repo name (e.g. github.io repos); only ".." is rejected.
	if _, err := Dir("octo-cat", "my.repo_v2", 3); err != nil {
		t.Errorf("Dir rejected a legitimate name: %v", err)
	}
}

func TestSaveUsesOwnerOnlyPermissions(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	if err := Save(&model.Report{PR: model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "x"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path, _ := file("o", "r", 1)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("cache file perm = %v, want %v", perm, filePerm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != dirPerm {
		t.Errorf("cache dir perm = %v, want %v", perm, dirPerm)
	}
}
