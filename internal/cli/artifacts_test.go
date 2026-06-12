package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/target"
)

// zipBytes builds an in-memory zip archive with the given name → content
// entries, for stubbing ArtifactArchive.
func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func runOptions() options {
	return options{reviewCommentLimit: 5, context: 10, shortThreshold: 100, tail: 100}
}

func TestRunReportListsArtifacts(t *testing.T) {
	s := &stubInspect{
		runInfo: model.RunInfo{Owner: "o", Repo: "r", RunID: 123},
		artifacts: []model.Artifact{
			{ID: 1, RunID: 123, Name: "coverage", SizeBytes: 2048, ExpiresAt: time.Now().Add(24 * time.Hour)},
		},
	}
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", RunID: 123}

	report, err := inspectWith(context.Background(), tgt, runOptions())
	if err != nil {
		t.Fatalf("inspectWith: %v", err)
	}
	if s.artifactsCalls != 1 {
		t.Errorf("RunArtifacts calls = %d, want 1", s.artifactsCalls)
	}
	if len(report.Artifacts) != 1 || report.Artifacts[0].Name != "coverage" {
		t.Fatalf("artifacts not listed: %+v", report.Artifacts)
	}
	if s.archiveCalls != 0 {
		t.Errorf("ArtifactArchive calls = %d, want 0 (no download requested)", s.archiveCalls)
	}
	if report.Artifacts[0].Path != "" {
		t.Errorf("path should be empty without a download, got %q", report.Artifacts[0].Path)
	}
}

func TestRunReportArtifactListingDegrades(t *testing.T) {
	s := &stubInspect{
		runInfo:      model.RunInfo{Owner: "o", Repo: "r", RunID: 123},
		artifactsErr: errors.New("boom"),
	}
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", RunID: 123}

	report, err := inspectWith(context.Background(), tgt, runOptions())
	if err != nil {
		t.Fatalf("listing failure without a download should be non-fatal: %v", err)
	}
	if len(report.Artifacts) != 0 {
		t.Errorf("artifacts should be empty after a listing failure: %+v", report.Artifacts)
	}
}

func TestDownloadArtifactsExtracts(t *testing.T) {
	dir := t.TempDir()
	s := &stubInspect{
		runInfo: model.RunInfo{Owner: "o", Repo: "r", RunID: 123},
		artifacts: []model.Artifact{
			{ID: 1, RunID: 123, Name: "coverage"},
			{ID: 2, RunID: 123, Name: "stale", Expired: true},
		},
		archives: map[int64][]byte{
			1: zipBytes(t, map[string]string{"nested/coverage.out": "mode: set\n"}),
		},
	}
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", RunID: 123}
	o := runOptions()
	o.artifactsDir = dir

	report, err := inspectWith(context.Background(), tgt, o)
	if err != nil {
		t.Fatalf("inspectWith: %v", err)
	}
	if s.archiveCalls != 1 {
		t.Errorf("ArtifactArchive calls = %d, want 1 (expired artifact must be skipped)", s.archiveCalls)
	}
	got, err := os.ReadFile(filepath.Join(dir, "coverage", "nested", "coverage.out"))
	if err != nil || string(got) != "mode: set\n" {
		t.Errorf("extracted file wrong: %q / %v", got, err)
	}
	if want := filepath.Join(dir, "coverage"); report.Artifacts[0].Path != want {
		t.Errorf("artifact path = %q, want %q", report.Artifacts[0].Path, want)
	}
	if report.Artifacts[1].Path != "" {
		t.Errorf("expired artifact should carry no path, got %q", report.Artifacts[1].Path)
	}
}

// A crafted entry name must not be able to write outside the artifact's
// directory: extraction goes through os.Root and fails closed.
func TestDownloadArtifactsZipSlip(t *testing.T) {
	dir := t.TempDir()
	s := &stubInspect{
		runInfo:   model.RunInfo{Owner: "o", Repo: "r", RunID: 123},
		artifacts: []model.Artifact{{ID: 1, RunID: 123, Name: "evil"}},
		archives: map[int64][]byte{
			1: zipBytes(t, map[string]string{"../escape.txt": "pwned"}),
		},
	}
	withStubInspect(t, s)
	tgt := target.Target{Owner: "o", Repo: "r", RunID: 123}
	o := runOptions()
	o.artifactsDir = filepath.Join(dir, "artifacts")

	if _, err := inspectWith(context.Background(), tgt, o); err == nil {
		t.Fatal("expected a traversal entry to fail the download")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); !os.IsNotExist(err) {
		t.Errorf("traversal entry escaped the artifact directory: %v", err)
	}
}

func TestDownloadArtifactsErrors(t *testing.T) {
	t.Run("listing failure is fatal with a download requested", func(t *testing.T) {
		s := &stubInspect{
			runInfo:      model.RunInfo{Owner: "o", Repo: "r", RunID: 123},
			artifactsErr: errors.New("boom"),
		}
		withStubInspect(t, s)
		o := runOptions()
		o.artifactsDir = t.TempDir()
		if _, err := inspectWith(context.Background(), target.Target{Owner: "o", Repo: "r", RunID: 123}, o); err == nil {
			t.Fatal("expected listing error to propagate")
		}
	})
	t.Run("archive failure is fatal", func(t *testing.T) {
		s := &stubInspect{
			runInfo:    model.RunInfo{Owner: "o", Repo: "r", RunID: 123},
			artifacts:  []model.Artifact{{ID: 1, Name: "coverage"}},
			archiveErr: errors.New("boom"),
		}
		withStubInspect(t, s)
		o := runOptions()
		o.artifactsDir = t.TempDir()
		if _, err := inspectWith(context.Background(), target.Target{Owner: "o", Repo: "r", RunID: 123}, o); err == nil {
			t.Fatal("expected archive error to propagate")
		}
	})
	t.Run("corrupt archive is fatal", func(t *testing.T) {
		s := &stubInspect{
			runInfo:   model.RunInfo{Owner: "o", Repo: "r", RunID: 123},
			artifacts: []model.Artifact{{ID: 1, Name: "coverage"}},
			archives:  map[int64][]byte{1: []byte("not a zip")},
		}
		withStubInspect(t, s)
		o := runOptions()
		o.artifactsDir = t.TempDir()
		if _, err := inspectWith(context.Background(), target.Target{Owner: "o", Repo: "r", RunID: 123}, o); err == nil {
			t.Fatal("expected corrupt-archive error to propagate")
		}
	})
	t.Run("PR target is rejected", func(t *testing.T) {
		s := ciStub()
		withStubInspect(t, s)
		o := runOptions()
		o.artifactsDir = t.TempDir()
		if _, err := inspectWith(context.Background(), target.Target{Owner: "o", Repo: "r", Number: 42}, o); err == nil {
			t.Fatal("expected --download-artifacts on a PR target to error")
		}
	})
	t.Run("offline is rejected", func(t *testing.T) {
		s := ciStub()
		withStubInspect(t, s)
		o := runOptions()
		o.artifactsDir = t.TempDir()
		o.offline = true
		if _, err := inspectWith(context.Background(), target.Target{Owner: "o", Repo: "r", Number: 42}, o); err == nil {
			t.Fatal("expected --download-artifacts with --offline to error")
		}
	})
}

func TestSafeArtifactName(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"coverage", "coverage"},
		{"a/b", "a_b"},
		{`a\b`, "a_b"},
		{"", "artifact"},
		{".", "artifact"},
		{"..", "artifact"},
	} {
		if got := safeArtifactName(tt.in); got != tt.want {
			t.Errorf("safeArtifactName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
