package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

const imagesFileName = "images.json"

// imageRecord is the persisted image listing for one owner, stamped with when it
// was fetched and the default-branch SHA it was keyed to so a caller can decide
// whether it is still fresh. The same record backs both a single-image resolve
// (one package) and a full owner listing (many).
type imageRecord struct {
	Registry   string               `json:"registry"`
	Owner      string               `json:"owner"`
	FetchedAt  time.Time            `json:"fetched_at"`
	DefaultSHA string               `json:"default_sha"`
	Packages   []model.ImagePackage `json:"packages"`
}

// ImageDir returns the cache directory for an owner's image listings:
// ~/.cache/shuck/images/<owner>. SHUCK_HOME overrides the base for testing.
func ImageDir(owner string) (string, error) {
	base, err := Base()
	if err != nil {
		return "", err
	}
	if err := safeSegment(owner); err != nil {
		return "", err
	}
	return filepath.Join(base, "images", owner), nil
}

func imageFile(owner string) (string, error) {
	dir, err := ImageDir(owner)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, imagesFileName), nil
}

// LoadImages reads the cached image packages for an owner, when they were
// fetched, and the default-branch SHA they were keyed to. A missing or corrupt
// cache returns ok=false.
func LoadImages(owner string) (pkgs []model.ImagePackage, defaultSHA string, fetchedAt time.Time, ok bool, err error) {
	path, err := imageFile(owner)
	if err != nil {
		return nil, "", time.Time{}, false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, "", time.Time{}, false, nil
	}
	if err != nil {
		return nil, "", time.Time{}, false, fmt.Errorf("read image cache %s: %w", path, err)
	}
	var rec imageRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		// A corrupt cache should not be fatal; treat it as a miss.
		return nil, "", time.Time{}, false, nil
	}
	return rec.Packages, rec.DefaultSHA, rec.FetchedAt, true, nil
}

// SaveImages writes the image packages for an owner, stamping the current time
// and the default-branch SHA it was keyed to, and creating directories as
// needed.
func SaveImages(registry, owner, defaultSHA string, pkgs []model.ImagePackage) error {
	path, err := imageFile(owner)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("create image cache dir: %w", err)
	}
	rec := imageRecord{Registry: registry, Owner: owner, FetchedAt: time.Now(), DefaultSHA: defaultSHA, Packages: pkgs}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("write image cache %s: %w", path, err)
	}
	return nil
}
