package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestImagesRoundTrip(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	want := []model.ImagePackage{
		{Owner: "acme", Name: "api", Versions: []model.ImageVersion{
			{Tags: []string{"v1.0.0", "latest"}, Digest: "sha256:abc", UpdatedAt: time.Unix(100, 0).UTC()},
		}},
	}
	before := time.Now().Truncate(time.Second)
	if err := SaveImages("ghcr.io", "acme", "deadbeef", want); err != nil {
		t.Fatalf("SaveImages: %v", err)
	}
	got, sha, fetchedAt, ok, err := LoadImages("acme")
	if err != nil || !ok {
		t.Fatalf("LoadImages ok=%v err=%v", ok, err)
	}
	if sha != "deadbeef" {
		t.Errorf("sha = %q, want deadbeef", sha)
	}
	if len(got) != 1 || got[0].Name != "api" || len(got[0].Versions) != 1 || got[0].Versions[0].Digest != "sha256:abc" {
		t.Errorf("packages = %+v", got)
	}
	if fetchedAt.Before(before) {
		t.Errorf("fetchedAt %v before save %v", fetchedAt, before)
	}
}

func TestLoadImagesMissing(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	_, _, _, ok, err := LoadImages("nobody")
	if err != nil || ok {
		t.Errorf("LoadImages(missing) ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

func TestImageDirRejectsTraversal(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	if _, err := ImageDir("../escape"); err == nil {
		t.Errorf("ImageDir(traversal) err=nil, want error")
	}
}

func TestPurgeSweepsImages(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHUCK_HOME", home)
	if err := SaveImages("ghcr.io", "acme", "sha", []model.ImagePackage{{Owner: "acme", Name: "api"}}); err != nil {
		t.Fatalf("SaveImages: %v", err)
	}
	path := filepath.Join(home, "images", "acme", imagesFileName)
	// Backdate the record so it is older than the TTL.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if err := Purge(time.Hour, ""); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("image cache dir still present after Purge: %v", err)
	}
}
