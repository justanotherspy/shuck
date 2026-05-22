// Package cache persists a PR's last inspection under ~/.shuck so repeat runs can
// skip redundant log downloads. The cache is advisory: callers always re-validate
// cheap metadata (head SHA, run/job listing) before trusting it.
package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/justanotherspy/shuck/internal/model"
)

const fileName = "cache.json"

// Dir returns the cache directory for a PR: ~/.shuck/cache/<owner>/<repo>/<pr>.
// SHUCK_HOME overrides the base (~/.shuck) for testing.
func Dir(owner, repo string, pr int) (string, error) {
	base := os.Getenv("SHUCK_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".shuck")
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write cache %s: %w", path, err)
	}
	return nil
}

// JobKey identifies a job attempt for cache reuse.
type JobKey struct {
	ID         int64
	RunAttempt int
}

// InspectedJobs indexes the inspected failed jobs of a cached report by
// (job id, run attempt) so a caller can reuse their drilled detail.
func InspectedJobs(cached *model.Report) map[JobKey]model.JobResult {
	out := map[JobKey]model.JobResult{}
	if cached == nil {
		return out
	}
	for _, j := range cached.FailedJobs {
		if j.Inspected {
			out[JobKey{ID: j.ID, RunAttempt: j.RunAttempt}] = j
		}
	}
	return out
}
