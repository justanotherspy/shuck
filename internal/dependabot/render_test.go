package dependabot

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func sampleReport() *model.DependabotReport {
	return &model.DependabotReport{
		Owner: "o", Repo: "r", ConfigSource: ".github/dependabot.yml", HasConfig: true,
		Detected: []model.DependabotEcosystem{
			{Ecosystem: "gomod", Directories: []string{"/"}, Covered: true},
			{Ecosystem: "npm", Directories: []string{"/web"}, Covered: false},
		},
		Findings: []model.DependabotFinding{
			{Level: model.DependabotWarning, Category: model.DependabotCategoryCoverage, Ecosystem: "npm", Directory: "/web", Message: "npm is used but has no update entry", Suggestion: "add an entry"},
			{Level: model.DependabotInfo, Category: model.DependabotCategoryBestPractice, Ecosystem: "gomod", Directory: "/", Message: "no cooldown", Suggestion: "add a cooldown"},
		},
	}
}

func TestRender(t *testing.T) {
	var b bytes.Buffer
	Render(&b, sampleReport())
	out := b.String()
	for _, want := range []string{
		"o/r — dependabot audit",
		"config: .github/dependabot.yml",
		"✓ gomod (/)",
		"✗ npm (/web)",
		"Summary: 2 finding(s) — 0 error, 1 warning, 1 info",
		"Coverage:",
		"Best practices:",
		"→ add a cooldown",
		"suggestion(s) to improve",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderClean(t *testing.T) {
	var b bytes.Buffer
	Render(&b, &model.DependabotReport{Owner: "o", Repo: "r", HasConfig: true, ConfigSource: "x"})
	if !strings.Contains(b.String(), "looks good — no findings") {
		t.Errorf("expected clean message:\n%s", b.String())
	}
}

func TestRenderNoConfigNoEcos(t *testing.T) {
	var b bytes.Buffer
	Render(&b, &model.DependabotReport{Owner: "o", Repo: "r"})
	out := b.String()
	if !strings.Contains(out, "config: (none)") || !strings.Contains(out, "(none found)") {
		t.Errorf("expected none markers:\n%s", out)
	}
}

func TestRenderErrors(t *testing.T) {
	var b bytes.Buffer
	r := &model.DependabotReport{Owner: "o", Repo: "r", Findings: []model.DependabotFinding{
		{Level: model.DependabotError, Category: model.DependabotCategoryConfig, Message: "no config"},
	}}
	Render(&b, r)
	if !strings.Contains(b.String(), "✗ 1 error(s)") {
		t.Errorf("expected error footer:\n%s", b.String())
	}
}

func TestEncodeJSON(t *testing.T) {
	var b bytes.Buffer
	if err := EncodeJSON(&b, sampleReport()); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	var doc Document
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.SchemaVersion != SchemaVersion {
		t.Errorf("schema = %d", doc.SchemaVersion)
	}
	if doc.OK {
		t.Error("OK should be false")
	}
	if doc.Summary.Total != 2 || doc.Summary.Warning != 1 || doc.Summary.Info != 1 {
		t.Errorf("summary = %+v", doc.Summary)
	}
	if len(doc.Ecosystems) != 2 || len(doc.Findings) != 2 {
		t.Errorf("slices wrong: %+v", doc)
	}
}

func TestNewDocumentNonNilSlices(t *testing.T) {
	doc := NewDocument(&model.DependabotReport{Owner: "o", Repo: "r"})
	if doc.Ecosystems == nil || doc.Findings == nil {
		t.Error("slices must be non-nil for [] serialization")
	}
	if !doc.OK {
		t.Error("empty report should be OK")
	}
}
