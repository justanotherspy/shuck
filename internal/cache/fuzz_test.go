package cache

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzCacheJobLogPath drives the cache's path builder with arbitrary owner/repo
// segments — the untrusted half of the input, arriving from CLI arguments,
// GitHub URLs, and git remotes. The invariant is containment rather than any
// particular spelling: whenever a path is produced it must be a local path
// under ~/.cache/shuck, since the monitor publishes it to an agent and
// SaveJobLog writes to it. It must also stay a pure query (nothing created) and
// agree with itself across calls, so a warm cache is actually found again.
func FuzzCacheJobLogPath(f *testing.F) {
	// Setenv has to happen out here: the testing package forbids it inside the
	// fuzz function.
	home := f.TempDir()
	f.Setenv("SHUCK_HOME", home)

	f.Add("o", "r", 7, int64(42), 1)
	f.Add("octo-cat", "my.repo_v2", 1, int64(1), 1)
	f.Add("..", "r", 1, int64(1), 1)
	f.Add("o", "../../../../etc/passwd", 1, int64(1), 1)
	f.Add("o/x", `..\..\win`, 0, int64(0), 0)
	f.Add("", "", -1, int64(-1), -1)
	f.Add("o\x00", "r", 1, int64(1), 1)

	f.Fuzz(func(t *testing.T, owner, repo string, pr int, jobID int64, attempt int) {
		path, err := JobLogPath(owner, repo, pr, jobID, attempt)
		if err != nil {
			if path != "" {
				t.Fatalf("JobLogPath(%q,%q,%d,%d,%d) failed but still named %q", owner, repo, pr, jobID, attempt, path)
			}
			return
		}

		rel, relErr := filepath.Rel(home, path)
		if relErr != nil || !filepath.IsLocal(rel) {
			t.Fatalf("JobLogPath(%q,%q,%d,%d,%d) = %q escapes cache base %q (rel=%q err=%v)",
				owner, repo, pr, jobID, attempt, path, home, rel, relErr)
		}

		again, againErr := JobLogPath(owner, repo, pr, jobID, attempt)
		if againErr != nil || again != path {
			t.Fatalf("JobLogPath(%q,%q,%d,%d,%d) not deterministic: %q then %q (err=%v)",
				owner, repo, pr, jobID, attempt, path, again, againErr)
		}

		// Reporting a location may not create one. Checked by looking at the
		// base rather than stat'ing the path itself: an over-long or otherwise
		// exotic segment makes os.Stat fail with something that is not
		// "not exist", which would be a spurious crash rather than a real one.
		entries, readErr := os.ReadDir(home)
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("JobLogPath(%q,%q,%d,%d,%d) created %d entries under %q (err=%v), want none",
				owner, repo, pr, jobID, attempt, len(entries), home, readErr)
		}
	})
}
