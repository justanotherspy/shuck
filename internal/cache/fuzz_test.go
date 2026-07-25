package cache

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// unsafeCacheSegment is an independent restatement of what an owner/repo
// segment may not be before it is joined into a cache path: empty, the "."
// spelling, ".." anywhere in it (not just as the whole segment), a separator
// that would smuggle extra levels past filepath.Join, or a NUL that truncates
// the name at the syscall boundary. It is deliberately a second copy of the
// rule rather than a call into safeSegment — an oracle that asks the code
// under test what it considers safe agrees with it no matter how far the guard
// is weakened.
func unsafeCacheSegment(s string) bool {
	return s == "" ||
		s == "." || s == ".." || strings.Contains(s, "..") ||
		strings.ContainsAny(s, `/\`) ||
		strings.ContainsRune(s, 0)
}

// FuzzCacheJobLogPath drives the cache's path builder with arbitrary owner/repo
// segments — the untrusted half of the input, arriving from CLI arguments,
// GitHub URLs, and git remotes. Two invariants, neither of which consults the
// production path builder: acceptance must track unsafeCacheSegment exactly,
// and an accepted target must spell out the documented layout component for
// component. Bare containment ("the result lands somewhere under the base") is
// not asserted, because filepath.Join makes it true by construction the moment
// separators are rejected — it would hold just as well if the tree were
// relaid out or owner and repo were swapped. The component equality also
// subsumes the determinism check this target used to make: it pins one exact
// string, where re-calling a pure function with identical arguments could not
// have disagreed. Reporting where a log would live must still create nothing.
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
	// Embedded "..", carrying no separator and no NUL: the only inputs the
	// dedicated Contains(s, "..") guard rejects on its own.
	f.Add("a..b", "r", 1, int64(1), 1)
	f.Add("o", "evil..", 1, int64(1), 1)
	f.Add("...", "r", 1, int64(1), 1)

	sep := string(filepath.Separator)

	f.Fuzz(func(t *testing.T, owner, repo string, pr int, jobID int64, attempt int) {
		path, err := JobLogPath(owner, repo, pr, jobID, attempt)

		reject := unsafeCacheSegment(owner) || unsafeCacheSegment(repo)
		if reject != (err != nil) {
			t.Fatalf("JobLogPath(%q,%q,%d,%d,%d) err=%v; want rejected=%v",
				owner, repo, pr, jobID, attempt, err, reject)
		}
		if err != nil {
			if path != "" {
				t.Fatalf("JobLogPath(%q,%q,%d,%d,%d) failed but still named %q", owner, repo, pr, jobID, attempt, path)
			}
			return
		}

		// Built by string concatenation rather than filepath.Join so the oracle
		// shares no code path with the production join.
		want := home + sep + strings.Join([]string{
			"cache", owner, repo, strconv.Itoa(pr), "logs",
			strconv.FormatInt(jobID, 10) + "-" + strconv.Itoa(attempt) + ".log",
		}, sep)
		if path != want {
			t.Fatalf("JobLogPath(%q,%q,%d,%d,%d) = %q, want %q",
				owner, repo, pr, jobID, attempt, path, want)
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
