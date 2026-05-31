package cache

import (
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
