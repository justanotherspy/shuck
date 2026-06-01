package compliance

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func sampleReport() *model.ComplianceReport {
	return &model.ComplianceReport{
		Owner: "justanotherspy", Repo: "shuck", ConfigSource: ".github/compliance.yml",
		Checks: []model.ComplianceCheck{
			{Category: "repository", Setting: "allow_merge_commit", Expected: "false", Actual: "false", Status: model.CompliancePass},
			{Category: "repository", Setting: "has_wiki", Expected: "false", Actual: "true", Status: model.ComplianceFail},
			{Category: "security", Setting: "secret_scanning", Expected: "enabled", Status: model.ComplianceSkipped, Message: "needs admin access"},
			{Category: "branch_protection", Setting: "main.enforce_admins", Expected: "true", Actual: "true", Status: model.CompliancePass},
		},
	}
}

func TestRenderContainsKeyFields(t *testing.T) {
	r := sampleReport()
	var b bytes.Buffer
	Render(&b, r)
	out := b.String()
	for _, want := range []string{
		"justanotherspy/shuck — compliance",
		"config: .github/compliance.yml",
		"Summary: 4 checked — 2 pass, 1 fail, 1 skipped",
		"Repository:",
		"✓ allow_merge_commit = false",
		"✗ has_wiki: want false, got true",
		"Security:",
		"– secret_scanning: want enabled — skipped (needs admin access)",
		"Branch protection:",
		"✓ main.enforce_admins = true",
		"✗ Not compliant — 1 setting(s) drifted from the config.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render output missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderCompliant(t *testing.T) {
	r := &model.ComplianceReport{
		Owner: "o", Repo: "r", ConfigSource: "cfg",
		Checks: []model.ComplianceCheck{
			{Category: "repository", Setting: "archived", Expected: "false", Actual: "false", Status: model.CompliancePass},
		},
	}
	var b bytes.Buffer
	Render(&b, r)
	if !strings.Contains(b.String(), "✓ Compliant — all settings match the config.") {
		t.Errorf("compliant footer missing:\n%s", b.String())
	}

	// With a skipped check the footer still reports compliant, but flags the skips.
	r.Checks = append(r.Checks, model.ComplianceCheck{
		Category: "security", Setting: "secret_scanning", Expected: "enabled",
		Status: model.ComplianceSkipped, Message: "needs admin",
	})
	b.Reset()
	Render(&b, r)
	if !strings.Contains(b.String(), "✓ Compliant (1 setting(s) skipped — not readable with this token).") {
		t.Errorf("compliant-with-skips footer missing:\n%s", b.String())
	}
}

func TestRenderNoChecks(t *testing.T) {
	r := &model.ComplianceReport{Owner: "o", Repo: "r"}
	var b bytes.Buffer
	Render(&b, r)
	if !strings.Contains(b.String(), "No checks declared in the config.") {
		t.Errorf("empty-config note missing:\n%s", b.String())
	}
}

func TestEncodeJSONShapeAndCounts(t *testing.T) {
	r := sampleReport()
	var b bytes.Buffer
	if err := EncodeJSON(&b, r); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	var doc Document
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b.String())
	}
	if doc.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", doc.SchemaVersion, SchemaVersion)
	}
	if doc.Compliant {
		t.Error("compliant = true, want false (one check failed)")
	}
	if doc.Summary.Total != 4 || doc.Summary.Pass != 2 || doc.Summary.Fail != 1 || doc.Summary.Skipped != 1 {
		t.Errorf("summary = %+v", doc.Summary)
	}
	if doc.Repo.Owner != "justanotherspy" || doc.Repo.Repo != "shuck" {
		t.Errorf("repo = %+v", doc.Repo)
	}
	if doc.ConfigSource != ".github/compliance.yml" {
		t.Errorf("config_source = %q", doc.ConfigSource)
	}
	if len(doc.Checks) != 4 {
		t.Errorf("checks = %d, want 4", len(doc.Checks))
	}
}

func TestEncodeJSONEmptyChecksNotNull(t *testing.T) {
	r := &model.ComplianceReport{Owner: "o", Repo: "r"}
	var b bytes.Buffer
	if err := EncodeJSON(&b, r); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	if !strings.Contains(b.String(), `"checks": []`) {
		t.Errorf("checks should serialize as [], got:\n%s", b.String())
	}
}

func sampleDiscovery() *Discovery {
	return &Discovery{
		Owner: "justanotherspy", Repo: "shuck", Path: ".github/compliance.yml",
		Data: []byte("repository:\n  has_wiki: false\n"),
	}
}

func TestRenderDiscoveryCreated(t *testing.T) {
	d := sampleDiscovery()
	d.Created, d.Changed = true, true
	d.Notes = []string{"security settings omitted: needs admin"}

	var b bytes.Buffer
	RenderDiscovery(&b, d, false)
	got := b.String()
	for _, want := range []string{
		"justanotherspy/shuck — compliance discover",
		"Created .github/compliance.yml from the live settings:",
		"– security settings omitted: needs admin",
		"has_wiki: false", // the new file is shown
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderDiscoveryUpdated(t *testing.T) {
	d := sampleDiscovery()
	d.Changed = true
	d.Changes = []Change{{Category: "repository", Setting: "has_wiki", From: "true", To: "false"}}

	var b bytes.Buffer
	RenderDiscovery(&b, d, false)
	got := b.String()
	for _, want := range []string{
		"Updated .github/compliance.yml — 1 declared setting(s) synced to the live values:",
		"~ repository.has_wiki: true → false",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "has_wiki: false\n") && strings.Contains(got, "repository:\n") {
		t.Errorf("a non-dry-run update should not dump the file contents:\n%s", got)
	}
}

func TestRenderDiscoveryUpToDate(t *testing.T) {
	var b bytes.Buffer
	RenderDiscovery(&b, sampleDiscovery(), false)
	if !strings.Contains(b.String(), "already matches the live settings") {
		t.Errorf("missing up-to-date message:\n%s", b.String())
	}
}

func TestRenderDiscoveryDryRun(t *testing.T) {
	d := sampleDiscovery()
	d.Changed = true
	d.Changes = []Change{{Category: "repository", Setting: "has_wiki", From: "true", To: "false"}}

	var b bytes.Buffer
	RenderDiscovery(&b, d, true)
	got := b.String()
	if !strings.Contains(got, "Would update") {
		t.Errorf("dry run should use the conditional verb:\n%s", got)
	}
	if !strings.Contains(got, "has_wiki: false") {
		t.Errorf("dry run should preview the file:\n%s", got)
	}
}

func TestEncodeDiscoveryJSON(t *testing.T) {
	d := sampleDiscovery()
	d.Changed = true
	d.Changes = []Change{{Category: "repository", Setting: "has_wiki", From: "true", To: "false"}}

	var b bytes.Buffer
	if err := EncodeDiscoveryJSON(&b, d); err != nil {
		t.Fatalf("EncodeDiscoveryJSON: %v", err)
	}
	var doc DiscoveryDocument
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b.String())
	}
	if doc.SchemaVersion != SchemaVersion || doc.Created || !doc.Updated || doc.UpToDate {
		t.Errorf("doc flags wrong: %+v", doc)
	}
	if len(doc.Changes) != 1 || doc.Changes[0].Setting != "has_wiki" {
		t.Errorf("changes = %+v", doc.Changes)
	}
	if doc.Notes == nil {
		t.Error("notes should serialize as [], not null")
	}
	if !strings.Contains(doc.Config, "has_wiki: false") {
		t.Errorf("config contents missing: %q", doc.Config)
	}
}

func TestNewDiscoveryDocumentUpToDate(t *testing.T) {
	doc := NewDiscoveryDocument(sampleDiscovery())
	if doc.Created || doc.Updated || !doc.UpToDate {
		t.Errorf("doc flags wrong: %+v", doc)
	}
	if doc.Changes == nil {
		t.Error("changes should serialize as [], not null")
	}
}
