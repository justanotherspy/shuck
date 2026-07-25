// Package cache persists a PR's last inspection under ~/.cache/shuck so repeat runs can
// skip redundant log downloads. The cache is advisory: callers always re-validate
// cheap metadata (head SHA, run/job listing) before trusting it.
package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

const fileName = "cache.json"

// logDirName is the subdirectory of a PR entry holding cached raw job logs.
// Both the writer (logFile) and the sweeper (newestEntryMod) key off it, so it
// is named once: a drift between the two would make Purge blind to the logs.
const logDirName = "logs"

// dirPerm/filePerm guard the cache: it can hold CI logs and security alert
// reports, which may quote secrets. Keeping ~/.cache/shuck owner-only avoids exposing
// that data to other local users on a shared machine.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// safeSegment validates a path segment (owner or repo) before it is joined into
// a cache path. owner/repo originate from user-supplied arguments, GitHub URLs,
// or a git remote, so a crafted value like "../../etc" must not be allowed to
// escape ~/.cache/shuck via filepath.Join. Real GitHub names never contain a path
// separator or "..", so this rejects exactly the traversal cases without
// constraining legitimate repositories.
func safeSegment(s string) error {
	if s == "" {
		return errors.New("empty path segment")
	}
	if s == "." || s == ".." || strings.Contains(s, "..") ||
		strings.ContainsAny(s, `/\`) || strings.ContainsRune(s, 0) {
		return fmt.Errorf("invalid path segment %q", s)
	}
	return nil
}

// Base returns shuck's base directory (~/.cache/shuck), honoring SHUCK_HOME. It
// is the parent of the per-PR cache and of cross-cutting state such as the
// version-check record. The default follows the XDG Base Directory spec via
// os.UserCacheDir (so XDG_CACHE_HOME is respected); SHUCK_HOME overrides it
// outright, chiefly for testing.
func Base() (string, error) {
	if base := os.Getenv("SHUCK_HOME"); base != "" {
		return base, nil
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate cache directory: %w", err)
	}
	return filepath.Join(cacheDir, "shuck"), nil
}

// Dir returns the cache directory for a PR: ~/.cache/shuck/cache/<owner>/<repo>/<pr>.
// SHUCK_HOME overrides the base (~/.cache/shuck) for testing.
func Dir(owner, repo string, pr int) (string, error) {
	base, err := Base()
	if err != nil {
		return "", err
	}
	if err := safeSegment(owner); err != nil {
		return "", err
	}
	if err := safeSegment(repo); err != nil {
		return "", err
	}
	return filepath.Join(base, "cache", owner, repo, strconv.Itoa(pr)), nil
}

func file(owner, repo string, pr int) (string, error) {
	dir, err := Dir(owner, repo, pr)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fileName), nil
}

// Load reads the cached report for a PR. A missing cache returns (nil, nil).
func Load(owner, repo string, pr int) (*model.Report, error) {
	path, err := file(owner, repo, pr)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cache %s: %w", path, err)
	}
	var r model.Report
	if err := json.Unmarshal(data, &r); err != nil {
		// A corrupt cache should not be fatal; treat it as a miss.
		return nil, nil
	}
	return &r, nil
}

// Save writes the report for its PR, creating directories as needed.
func Save(r *model.Report) error {
	path, err := file(r.PR.Owner, r.PR.Repo, r.PR.Number)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("write cache %s: %w", path, err)
	}
	return nil
}

// logFile returns the path of a job attempt's raw log within a PR's cache:
// ~/.cache/shuck/cache/<owner>/<repo>/<pr>/logs/<jobID>-<attempt>.log.
func logFile(owner, repo string, pr int, jobID int64, attempt int) (string, error) {
	dir, err := Dir(owner, repo, pr)
	if err != nil {
		return "", err
	}
	name := strconv.FormatInt(jobID, 10) + "-" + strconv.Itoa(attempt) + ".log"
	return filepath.Join(dir, logDirName, name), nil
}

// JobLogPath reports where a job attempt's raw log is cached, whether or not
// it has been written yet. The background monitor puts this path in a CI
// failure event so an agent handed a trimmed excerpt can read the whole log
// off disk instead of going back to GitHub for it.
func JobLogPath(owner, repo string, pr int, jobID int64, attempt int) (string, error) {
	return logFile(owner, repo, pr, jobID, attempt)
}

// SaveJobLog persists a job attempt's whole raw log so a later run can re-parse
// it locally (e.g. under different context flags) without re-downloading.
func SaveJobLog(owner, repo string, pr int, jobID int64, attempt int, raw string) error {
	path, err := logFile(owner, repo, pr, jobID, attempt)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("create log cache dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(raw), filePerm); err != nil {
		return fmt.Errorf("write log cache %s: %w", path, err)
	}
	return nil
}

// LoadJobLog reads a job attempt's cached raw log. A missing log returns
// ok=false.
func LoadJobLog(owner, repo string, pr int, jobID int64, attempt int) (raw string, ok bool, err error) {
	path, err := logFile(owner, repo, pr, jobID, attempt)
	if err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read log cache %s: %w", path, err)
	}
	return string(data), true, nil
}

// Purge best-effort removes cache entries that have not been refreshed within
// ttl, skipping keep (the absolute directory of the current target so an
// in-flight run is never evicted), and removes the whole entry directory when
// it goes. Errors are returned for visibility but callers treat purging as
// advisory.
//
// The action and security caches hold exactly one record file per entry, so
// that record's mtime is the entry's age. A PR entry is not that shape: the
// background monitor writes whole raw job logs under <pr>/logs/ but never
// writes cache.json (it distills events, it has no model.Report to persist), so
// aging a PR entry by its record alone leaves a monitor-only PR growing logs on
// disk that nothing ever reclaims. A PR entry's age is therefore the newest
// mtime across cache.json *and* its cached logs.
func Purge(ttl time.Duration, keep string) error {
	base, err := Base()
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-ttl)
	purgePRs(filepath.Join(base, "cache"), cutoff, keep)
	for _, r := range []struct{ root, record string }{
		{"actions", actionsFileName},
		{"security", securityFileName},
	} {
		rootDir := filepath.Join(base, r.root)
		_ = filepath.WalkDir(rootDir, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() || d.Name() != r.record {
				return nil
			}
			info, err := d.Info()
			if err != nil || info.ModTime().After(cutoff) {
				return nil
			}
			removeEntry(filepath.Dir(path), keep)
			return nil
		})
	}
	return nil
}

// purgePRs sweeps the PR cache root, whose entries are the
// <owner>/<repo>/<pr> directories Dir builds.
func purgePRs(root string, cutoff time.Time, keep string) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return nil
		}
		// Owner and repo levels hold no entry of their own; keep descending.
		if depth := strings.Count(filepath.ToSlash(rel), "/") + 1; depth < 3 {
			return nil
		}
		newest, ok := newestEntryMod(path)
		// An entry with nothing timestamped in it is either an empty leftover
		// or a directory SaveJobLog has created but not yet written into.
		// Removing it would race that write, and an empty directory costs
		// nothing to leave.
		if ok && !newest.After(cutoff) {
			removeEntry(path, keep)
		}
		// The entry's contents were just read; there is nothing below it to
		// sweep, and descending into a directory that may have been removed
		// only produces walk errors.
		return filepath.SkipDir
	})
}

// newestEntryMod reports the most recent modification time among a PR entry's
// record file and its cached raw job logs, and whether it found either.
func newestEntryMod(entryDir string) (time.Time, bool) {
	var (
		newest time.Time
		found  bool
	)
	note := func(path string) {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return
		}
		found = true
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	note(filepath.Join(entryDir, fileName))
	logDir := filepath.Join(entryDir, logDirName)
	logs, err := os.ReadDir(logDir)
	if err != nil {
		return newest, found
	}
	for _, l := range logs {
		note(filepath.Join(logDir, l.Name()))
	}
	return newest, found
}

// removeEntry deletes a whole cache entry unless it is the caller's current
// target.
func removeEntry(entryDir, keep string) {
	if entryDir == keep {
		return
	}
	// entryDir is always under the user-owned cache base resolved by Base and
	// only ever names a cache entry, so this is not an attacker-controlled
	// traversal target.
	_ = os.RemoveAll(entryDir)
}
