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
