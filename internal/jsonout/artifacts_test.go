package jsonout

import (
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestNewDocumentArtifacts(t *testing.T) {
	created := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	expires := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	r := &model.Report{
		Run: &model.RunInfo{Owner: "o", Repo: "r", RunID: 123},
		Artifacts: []model.Artifact{
			{ID: 1, RunID: 123, Name: "coverage", SizeBytes: 2048, CreatedAt: created, ExpiresAt: expires, Path: "/tmp/arts/coverage"},
			{ID: 2, RunID: 123, Name: "stale", SizeBytes: 7, Expired: true, CreatedAt: created, ExpiresAt: created},
		},
	}

	doc := NewDocument(r)
	if len(doc.Artifacts) != 2 {
		t.Fatalf("artifacts = %d, want 2", len(doc.Artifacts))
	}
	a := doc.Artifacts[0]
	if a.ID != 1 || a.RunID != 123 || a.Name != "coverage" || a.SizeBytes != 2048 || a.Expired {
		t.Errorf("artifact[0] = %+v", a)
	}
	if a.CreatedAt != "2026-06-01T10:00:00Z" || a.ExpiresAt != "2026-09-01T10:00:00Z" {
		t.Errorf("timestamps = %q / %q", a.CreatedAt, a.ExpiresAt)
	}
	if a.Path != "/tmp/arts/coverage" {
		t.Errorf("path = %q", a.Path)
	}
	if !doc.Artifacts[1].Expired || doc.Artifacts[1].Path != "" {
		t.Errorf("artifact[1] = %+v", doc.Artifacts[1])
	}
}

// PR reports carry no artifacts; the field must be omitted, not emitted as [].
func TestNewDocumentNoArtifactsOmitted(t *testing.T) {
	doc := NewDocument(&model.Report{PR: model.PR{Owner: "o", Repo: "r", Number: 1}})
	if doc.Artifacts != nil {
		t.Errorf("artifacts should be nil for a PR report, got %+v", doc.Artifacts)
	}
}
