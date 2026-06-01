package cache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

// TestLoadCorrupt confirms a corrupt PR cache is treated as a miss (nil, nil)
// rather than an error.
func TestLoadCorrupt(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	path, err := file("o", "r", 1)
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), filePerm); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := Load("o", "r", 1)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != nil {
		t.Errorf("corrupt cache should be a miss, got %+v", got)
	}
}

// Save / SaveJobLog reject a path-traversal owner before writing anything,
// surfacing the safeSegment error.
func TestSaveRejectsBadSegment(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	if err := Save(&model.Report{PR: model.PR{Owner: "../evil", Repo: "r", Number: 1}}); err == nil {
		t.Error("Save with a traversal owner err=nil, want error")
	}
	if err := SaveJobLog("../evil", "r", 1, 9, 1, "log"); err == nil {
		t.Error("SaveJobLog with a traversal owner err=nil, want error")
	}
}

// Load / LoadJobLog also reject a bad segment up front.
func TestLoadRejectsBadSegment(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	if _, err := Load("../evil", "r", 1); err == nil {
		t.Error("Load with a traversal owner err=nil, want error")
	}
	if _, _, err := LoadJobLog("../evil", "r", 1, 9, 1); err == nil {
		t.Error("LoadJobLog with a traversal owner err=nil, want error")
	}
}

// When the cache root is occupied by a regular file, MkdirAll fails and the
// Save functions surface a "create … dir" error.
func TestSaveMkdirFailures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHUCK_HOME", home)
	// Occupy every top-level cache root with a regular file so MkdirAll under it
	// cannot create the needed subdirectories.
	for _, root := range []string{"cache", "actions", "images", "security"} {
		if err := os.WriteFile(filepath.Join(home, root), []byte("x"), filePerm); err != nil {
			t.Fatalf("seed %s file: %v", root, err)
		}
	}
	if err := Save(&model.Report{PR: model.PR{Owner: "o", Repo: "r", Number: 1}}); err == nil {
		t.Error("Save with an unwritable cache root err=nil, want error")
	}
	if err := SaveJobLog("o", "r", 1, 9, 1, "log"); err == nil {
		t.Error("SaveJobLog with an unwritable cache root err=nil, want error")
	}
	if err := SaveActionTags("o", "r", "sha", nil); err == nil {
		t.Error("SaveActionTags with an unwritable cache root err=nil, want error")
	}
	if err := SaveImages("ghcr.io", "o", "sha", nil); err == nil {
		t.Error("SaveImages with an unwritable cache root err=nil, want error")
	}
	if err := SaveSecurityReport(&model.SecurityReport{Owner: "o", Repo: "r", State: "open"}, "sha"); err == nil {
		t.Error("SaveSecurityReport with an unwritable cache root err=nil, want error")
	}
}

// LoadActionTags treats corrupt JSON as a miss.
func TestLoadActionTagsCorrupt(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	path, err := actionFile("actions", "checkout")
	if err != nil {
		t.Fatalf("actionFile: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("not json"), filePerm); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, _, ok, err := LoadActionTags("actions", "checkout")
	if err != nil {
		t.Fatalf("LoadActionTags: %v", err)
	}
	if ok {
		t.Error("expected ok=false for corrupt action cache")
	}
}

// SaveActionTags / LoadActionTags reject a bad segment up front.
func TestActionTagsRejectBadSegment(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	if err := SaveActionTags("../evil", "checkout", "sha", nil); err == nil {
		t.Error("SaveActionTags with a traversal owner err=nil, want error")
	}
	if _, _, _, _, err := LoadActionTags("../evil", "checkout"); err == nil {
		t.Error("LoadActionTags with a traversal owner err=nil, want error")
	}
}

// LoadImages treats corrupt JSON as a miss.
func TestLoadImagesCorrupt(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	path, err := imageFile("acme")
	if err != nil {
		t.Fatalf("imageFile: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("}{"), filePerm); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, _, ok, err := LoadImages("acme")
	if err != nil {
		t.Fatalf("LoadImages: %v", err)
	}
	if ok {
		t.Error("expected ok=false for corrupt image cache")
	}
}

// SaveImages / LoadImages reject a bad segment up front.
func TestImagesRejectBadSegment(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	if err := SaveImages("ghcr.io", "../evil", "sha", nil); err == nil {
		t.Error("SaveImages with a traversal owner err=nil, want error")
	}
	if _, _, _, _, err := LoadImages("../evil"); err == nil {
		t.Error("LoadImages with a traversal owner err=nil, want error")
	}
}
