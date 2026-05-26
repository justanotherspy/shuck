package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

const actionsFileName = "tags.json"

// actionTagsRecord is the persisted tag list for one action repository, stamped
// with when it was fetched so a caller can decide whether it is still fresh.
type actionTagsRecord struct {
	Owner     string            `json:"owner"`
	Repo      string            `json:"repo"`
	FetchedAt time.Time         `json:"fetched_at"`
	Tags      []model.ActionTag `json:"tags"`
}

// ActionDir returns the cache directory for an action repository's tags:
// ~/.shuck/actions/<owner>/<repo>. SHUCK_HOME overrides the base for testing.
func ActionDir(owner, repo string) (string, error) {
	base, err := Base()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "actions", owner, repo), nil
}

func actionFile(owner, repo string) (string, error) {
	dir, err := ActionDir(owner, repo)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, actionsFileName), nil
}

// LoadActionTags reads the cached tag list for an action repository and when it
// was fetched. A missing or corrupt cache returns ok=false.
func LoadActionTags(owner, repo string) (tags []model.ActionTag, fetchedAt time.Time, ok bool, err error) {
	path, err := actionFile(owner, repo)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("read action cache %s: %w", path, err)
	}
	var rec actionTagsRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		// A corrupt cache should not be fatal; treat it as a miss.
		return nil, time.Time{}, false, nil
	}
	return rec.Tags, rec.FetchedAt, true, nil
}

// SaveActionTags writes the tag list for an action repository, stamping the
// current time, and creating directories as needed.
func SaveActionTags(owner, repo string, tags []model.ActionTag) error {
	path, err := actionFile(owner, repo)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create action cache dir: %w", err)
	}
	rec := actionTagsRecord{Owner: owner, Repo: repo, FetchedAt: time.Now(), Tags: tags}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write action cache %s: %w", path, err)
	}
	return nil
}
