package security

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func sampleReport() *model.SecurityReport {
	return &model.SecurityReport{
		Owner: "justanotherspy", Repo: "shuck", State: "open",
		CodeScanning:   model.SecuritySource{Status: model.StatusOK},
		SecretScanning: model.SecuritySource{Status: model.StatusDisabled, Message: "not enabled or no access"},
		Dependabot:     model.SecuritySource{Status: model.StatusOK},
		CodeScanningAlerts: []model.CodeScanningAlert{
			{Number: 7, State: "open", Severity: model.SeverityHigh, RuleID: "py/sql-injection", Tool: "CodeQL", Path: "app/db/users.py", StartLine: 42, EndLine: 45, Message: "User-provided value.", HTMLURL: "https://example/cs/7"},
		},
		DependabotAlerts: []model.DependabotAlert{
			{Number: 9, State: "open", Severity: model.SeverityHigh, Ecosystem: "pip", Package: "django", VulnerableVersions: "< 3.2.4", FixedVersion: "3.2.4", GHSAID: "GHSA-x", CVEID: "CVE-2021-1", Summary: "Traversal", HTMLURL: "https://example/dep/9"},
			{Number: 12, State: "open", Severity: model.SeverityCritical, Ecosystem: "npm", Package: "lodash", VulnerableVersions: "< 4.17.21", FixedVersion: "4.17.21", GHSAID: "GHSA-jf85", CVEID: "CVE-2019-10744", Summary: "Prototype pollution", ManifestPath: "package-lock.json", HTMLURL: "https://example/dep/12"},
		},
	}
}

func TestSortBySeverityThenNumber(t *testing.T) {
	r := sampleReport()
	Sort(r)
	if r.DependabotAlerts[0].Number != 12 {
		t.Errorf("critical alert should sort first, got #%d", r.DependabotAlerts[0].Number)
	}
	if r.DependabotAlerts[1].Number != 9 {
		t.Errorf("high alert should sort after critical, got #%d", r.DependabotAlerts[1].Number)
	}
}

func TestRenderContainsKeyFields(t *testing.T) {
	r := sampleReport()
	Sort(r)
	var b bytes.Buffer
	Render(&b, r)
	out := b.String()
	for _, want := range []string{
		"justanotherspy/shuck — security alerts (open)",
		"Summary: 3 alerts — 1 critical, 2 high",
		"Dependabot (2):",
		"● critical  npm  lodash → 4.17.21",
		"GHSA-jf85",
		"CVE-2019-10744",
		"manifest: package-lock.json",
		"Code scanning (1):",
		"● high  py/sql-injection   [CodeQL]",
		"app/db/users.py:42-45",
		"Secret scanning: not enabled or no access — skipped.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render output missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderCleanRepo(t *testing.T) {
	r := &model.SecurityReport{
		Owner: "o", Repo: "r", State: "open",
		CodeScanning:   model.SecuritySource{Status: model.StatusOK},
		SecretScanning: model.SecuritySource{Status: model.StatusOK},
		Dependabot:     model.SecuritySource{Status: model.StatusOK},
	}
	var b bytes.Buffer
	Render(&b, r)
	out := b.String()
	if !strings.Contains(out, "Summary: no open alerts") || !strings.Contains(out, "✓ No open security alerts.") {
		t.Errorf("clean repo output unexpected:\n%s", out)
	}
}

func TestEncodeJSONShapeAndCounts(t *testing.T) {
	r := sampleReport()
	Sort(r)
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
	if doc.Summary.Total != 3 {
		t.Errorf("summary.total = %d, want 3", doc.Summary.Total)
	}
	if doc.Summary.BySeverity["critical"] != 1 || doc.Summary.BySeverity["high"] != 2 {
		t.Errorf("by_severity = %v", doc.Summary.BySeverity)
	}
	if doc.Sources.SecretScanning.Status != model.StatusDisabled {
		t.Errorf("secret scanning source = %q, want disabled", doc.Sources.SecretScanning.Status)
	}
}

func TestEncodeJSONEmptySlicesNotNull(t *testing.T) {
	r := &model.SecurityReport{Owner: "o", Repo: "r", State: "open"}
	var b bytes.Buffer
	if err := EncodeJSON(&b, r); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		`"code_scanning_alerts": []`,
		`"secret_scanning_alerts": []`,
		`"dependabot_alerts": []`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// TestSecretValueNeverRendered guards the redaction invariant: a leaked secret's
// value must never appear in text or JSON output. The model has no field for it,
// so this is a regression guard on the type as much as the renderers.
func TestSecretValueNeverRendered(t *testing.T) {
	const secretVal = "ghp_SUPERSECRETVALUE1234567890"
	r := &model.SecurityReport{
		Owner: "o", Repo: "r", State: "open",
		SecretScanning: model.SecuritySource{Status: model.StatusOK},
		SecretScanningAlerts: []model.SecretScanningAlert{{
			Number: 1, State: "open", SecretType: "github_personal_access_token",
			DisplayName: "GitHub Personal Access Token",
			Locations:   []model.SecretLocation{{Path: ".env", StartLine: 1, EndLine: 1}},
			HTMLURL:     "https://example/secret/1",
		}},
	}
	var text bytes.Buffer
	Render(&text, r)
	var jsonb bytes.Buffer
	if err := EncodeJSON(&jsonb, r); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	if strings.Contains(text.String(), secretVal) || strings.Contains(jsonb.String(), secretVal) {
		t.Fatal("raw secret value leaked into output")
	}
	if !strings.Contains(text.String(), "GitHub Personal Access Token") || !strings.Contains(text.String(), ".env:1") {
		t.Errorf("secret metadata missing from output:\n%s", text.String())
	}
}
