package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestSecurityReportRoundTrip(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())

	want := &model.SecurityReport{
		Owner:        "acme",
		Repo:         "api",
		State:        "open",
		CodeScanning: model.SecuritySource{Status: model.StatusOK},
		DependabotAlerts: []model.DependabotAlert{
			{Number: 1, State: "open", Severity: model.SeverityHigh, Package: "left-pad"},
		},
	}
	before := time.Now().Truncate(time.Second)
	if err := SaveSecurityReport(want, "deadbeef"); err != nil {
		t.Fatalf("SaveSecurityReport: %v", err)
	}

	got, sha, fetchedAt, ok, err := LoadSecurityReport("acme", "api", "open")
	if err != nil {
		t.Fatalf("LoadSecurityReport: %v", err)
	}
	if !ok {
		t.Fatal("LoadSecurityReport ok=false for a saved report")
	}
	if sha != "deadbeef" {
		t.Errorf("sha = %q, want deadbeef", sha)
	}
	if got == nil || got.Owner != "acme" || got.TotalAlerts() != 1 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if fetchedAt.Before(before) {
		t.Errorf("fetchedAt %v before save %v", fetchedAt, before)
	}
}

func TestLoadSecurityReportStateMismatch(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	if err := SaveSecurityReport(&model.SecurityReport{Owner: "acme", Repo: "api", State: "open"}, "sha"); err != nil {
		t.Fatalf("SaveSecurityReport: %v", err)
	}
	// A different requested state must miss (ok=false), without error.
	_, _, _, ok, err := LoadSecurityReport("acme", "api", "all")
	if err != nil {
		t.Fatalf("LoadSecurityReport: %v", err)
	}
	if ok {
		t.Error("expected ok=false for a state mismatch")
	}
}

func TestLoadSecurityReportMissing(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	_, _, _, ok, err := LoadSecurityReport("nobody", "nothing", "open")
	if err != nil {
		t.Fatalf("LoadSecurityReport: %v", err)
	}
	if ok {
		t.Error("expected ok=false for a missing report")
	}
}

func TestLoadSecurityReportCorrupt(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	path, err := securityFile("acme", "api")
	if err != nil {
		t.Fatalf("securityFile: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not valid json"), filePerm); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	// Corrupt JSON is treated as a miss, not an error.
	_, _, _, ok, err := LoadSecurityReport("acme", "api", "open")
	if err != nil {
		t.Fatalf("LoadSecurityReport: %v", err)
	}
	if ok {
		t.Error("expected ok=false for corrupt JSON")
	}
}

// A well-formed record whose report field is null is also treated as a miss.
func TestLoadSecurityReportNullReport(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	path, err := securityFile("acme", "api")
	if err != nil {
		t.Fatalf("securityFile: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"state":"open","report":null}`), filePerm); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, _, ok, err := LoadSecurityReport("acme", "api", "open")
	if err != nil {
		t.Fatalf("LoadSecurityReport: %v", err)
	}
	if ok {
		t.Error("expected ok=false for a null report")
	}
}

func TestSecurityDirRejectsTraversal(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	if _, err := SecurityDir("../escape", "r"); err == nil {
		t.Error("SecurityDir(traversal owner) err=nil, want error")
	}
	if _, err := SecurityDir("o", "../escape"); err == nil {
		t.Error("SecurityDir(traversal repo) err=nil, want error")
	}
}

// SaveSecurityReport surfaces the bad-segment error from its path build rather
// than writing anything.
func TestSaveSecurityReportRejectsTraversal(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	if err := SaveSecurityReport(&model.SecurityReport{Owner: "../evil", Repo: "r", State: "open"}, "sha"); err == nil {
		t.Error("SaveSecurityReport with a traversal owner err=nil, want error")
	}
}
