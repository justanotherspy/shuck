package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

const securityFileName = "alerts.json"

// securityRecord is the persisted security report for one repository, stamped
// with when it was fetched and for which state filter, so a caller can decide
// whether it is still fresh and matches the requested state.
type securityRecord struct {
	Owner      string                `json:"owner"`
	Repo       string                `json:"repo"`
	State      string                `json:"state"`
	FetchedAt  time.Time             `json:"fetched_at"`
	DefaultSHA string                `json:"default_sha"`
	Report     *model.SecurityReport `json:"report"`
}

// SecurityDir returns the cache directory for a repo's security alerts:
// ~/.shuck/security/<owner>/<repo>. SHUCK_HOME overrides the base for testing.
func SecurityDir(owner, repo string) (string, error) {
	base, err := Base()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "security", owner, repo), nil
}

func securityFile(owner, repo string) (string, error) {
	dir, err := SecurityDir(owner, repo)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, securityFileName), nil
}

// LoadSecurityReport reads the cached report for a repo, when it was fetched,
// and the default-branch SHA it was keyed to. A missing or corrupt cache, or one
// fetched for a different state, returns ok=false.
func LoadSecurityReport(owner, repo, state string) (report *model.SecurityReport, defaultSHA string, fetchedAt time.Time, ok bool, err error) {
	path, err := securityFile(owner, repo)
	if err != nil {
		return nil, "", time.Time{}, false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, "", time.Time{}, false, nil
	}
	if err != nil {
		return nil, "", time.Time{}, false, fmt.Errorf("read security cache %s: %w", path, err)
	}
	var rec securityRecord
	if err := json.Unmarshal(data, &rec); err != nil || rec.Report == nil {
		// A corrupt cache should not be fatal; treat it as a miss.
		return nil, "", time.Time{}, false, nil
	}
	if rec.State != state {
		return nil, "", time.Time{}, false, nil
	}
	return rec.Report, rec.DefaultSHA, rec.FetchedAt, true, nil
}

// SaveSecurityReport writes the report for a repo, stamping the current time, the
// state it was fetched for, and the default-branch SHA it was keyed to, and
// creating directories as needed.
func SaveSecurityReport(r *model.SecurityReport, defaultSHA string) error {
	path, err := securityFile(r.Owner, r.Repo)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create security cache dir: %w", err)
	}
	rec := securityRecord{Owner: r.Owner, Repo: r.Repo, State: r.State, FetchedAt: time.Now(), DefaultSHA: defaultSHA, Report: r}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write security cache %s: %w", path, err)
	}
	return nil
}
