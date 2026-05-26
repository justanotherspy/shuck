package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/jsonout"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/security"
	"github.com/justanotherspy/shuck/internal/target"
)

func failingReport() *model.Report {
	return &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 42, Title: "fix parser", HeadSHA: "abc1234"},
		FailedJobs: []model.JobResult{{ID: 1, Name: "build", Conclusion: "failure",
			FailedSteps: []model.FailedStep{{Number: 2, Name: "Run tests", Excerpt: "boom"}}}},
	}
}

func okSecurityReport() *model.SecurityReport {
	return &model.SecurityReport{
		Owner: "o", Repo: "r", State: "open",
		CodeScanning:   model.SecuritySource{Status: model.StatusOK},
		SecretScanning: model.SecuritySource{Status: model.StatusOK},
		Dependabot:     model.SecuritySource{Status: model.StatusOK},
		DependabotAlerts: []model.DependabotAlert{{Number: 1, State: "open",
			Severity: model.SeverityCritical, Ecosystem: "npm", Package: "lodash", FixedVersion: "4.17.21"}},
	}
}

func TestEmitAllCombinedText(t *testing.T) {
	res := &combinedResult{report: failingReport(), sec: okSecurityReport()}
	var out strings.Builder
	code, err := emitAll(&out, res, false)
	if err != nil {
		t.Fatalf("emitAll: %v", err)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1 (CI failures present)", code)
	}
	got := out.String()
	for _, want := range []string{"build", "fix parser", "security alerts (open)", "lodash"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in combined output:\n%s", want, got)
		}
	}
}

func TestEmitAllCombinedJSON(t *testing.T) {
	res := &combinedResult{report: failingReport(), sec: okSecurityReport()}
	var out strings.Builder
	if _, err := emitAll(&out, res, true); err != nil {
		t.Fatalf("emitAll: %v", err)
	}
	var doc struct {
		SchemaVersion int                `json:"schema_version"`
		Inspection    jsonout.Document   `json:"inspection"`
		Security      *security.Document `json:"security"`
		SecurityError string             `json:"security_error"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("not a combined document: %v\n%s", err, out.String())
	}
	if doc.SchemaVersion != combinedSchemaVersion {
		t.Errorf("schema_version = %d, want %d", doc.SchemaVersion, combinedSchemaVersion)
	}
	if doc.Inspection.Summary.Failed != 1 {
		t.Errorf("inspection.summary.failed = %d, want 1", doc.Inspection.Summary.Failed)
	}
	if doc.Security == nil || doc.Security.State != "open" {
		t.Errorf("security sub-document wrong: %+v", doc.Security)
	}
	if doc.SecurityError != "" {
		t.Errorf("unexpected security_error %q", doc.SecurityError)
	}
}

func TestEmitAllSecurityError(t *testing.T) {
	res := &combinedResult{report: failingReport(), secErr: errors.New("forbidden")}
	var out strings.Builder
	code, err := emitAll(&out, res, false)
	if err != nil {
		t.Fatalf("emitAll: %v", err)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1 (CI verdict, not security)", code)
	}
	if !strings.Contains(out.String(), "security alerts: unavailable (forbidden)") {
		t.Errorf("missing degraded note:\n%s", out.String())
	}

	out.Reset()
	if _, err := emitAll(&out, res, true); err != nil {
		t.Fatalf("emitAll json: %v", err)
	}
	var doc struct {
		Security      *security.Document `json:"security"`
		SecurityError string             `json:"security_error"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("json: %v", err)
	}
	if doc.Security != nil || doc.SecurityError != "forbidden" {
		t.Errorf("want security=nil security_error=forbidden, got %+v / %q", doc.Security, doc.SecurityError)
	}
}

// TestEmitAllNoSecurityHalfPlain proves run/offline targets (no security half)
// fall back to the plain single document, not the combined envelope.
func TestEmitAllNoSecurityHalfPlain(t *testing.T) {
	res := &combinedResult{report: failingReport()}
	var out strings.Builder
	if _, err := emitAll(&out, res, true); err != nil {
		t.Fatalf("emitAll: %v", err)
	}
	if strings.Contains(out.String(), "\"inspection\"") {
		t.Errorf("plain output should not be a combined envelope:\n%s", out.String())
	}
	var doc jsonout.Document
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("not a plain jsonout.Document: %v\n%s", err, out.String())
	}
	if doc.Summary.Failed != 1 {
		t.Errorf("summary.failed = %d, want 1", doc.Summary.Failed)
	}
}

func TestWithSecurityGating(t *testing.T) {
	withStubSecurity(t, okStub())
	report := failingReport()
	ctx := context.Background()

	pr := target.Target{Owner: "o", Repo: "r", Number: 42}
	if res := withSecurity(ctx, pr, options{state: "open"}, report); res.sec == nil || res.secErr != nil {
		t.Errorf("PR target should attach security, got sec=%v err=%v", res.sec, res.secErr)
	}

	run := target.Target{Owner: "o", Repo: "r", RunID: 5}
	if res := withSecurity(ctx, run, options{state: "open"}, report); res.sec != nil || res.secErr != nil {
		t.Errorf("run target should skip security, got sec=%v", res.sec)
	}

	if res := withSecurity(ctx, pr, options{state: "open", offline: true}, report); res.sec != nil {
		t.Errorf("offline should skip security, got sec=%v", res.sec)
	}
}
