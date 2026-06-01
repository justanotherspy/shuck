package security

import (
	"bytes"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestSortAllThreeSources(t *testing.T) {
	r := &model.SecurityReport{
		Owner: "o", Repo: "r", State: "open",
		CodeScanningAlerts: []model.CodeScanningAlert{
			{Number: 5, Severity: model.SeverityLow, RuleID: "a"},
			{Number: 2, Severity: model.SeverityCritical, RuleID: "b"},
			{Number: 9, Severity: model.SeverityCritical, RuleID: "c"}, // tie on severity → by number
		},
		DependabotAlerts: []model.DependabotAlert{
			{Number: 3, Severity: model.SeverityMedium},
			{Number: 1, Severity: model.SeverityHigh},
		},
		SecretScanningAlerts: []model.SecretScanningAlert{
			{Number: 8, SecretType: "t1"},
			{Number: 2, SecretType: "t2"},
			{Number: 5, SecretType: "t3"},
		},
	}
	Sort(r)

	// Code scanning: critical first, ties broken by ascending number.
	if r.CodeScanningAlerts[0].Number != 2 || r.CodeScanningAlerts[1].Number != 9 || r.CodeScanningAlerts[2].Number != 5 {
		t.Errorf("code scanning order = %d,%d,%d, want 2,9,5",
			r.CodeScanningAlerts[0].Number, r.CodeScanningAlerts[1].Number, r.CodeScanningAlerts[2].Number)
	}
	// Dependabot: high before medium.
	if r.DependabotAlerts[0].Number != 1 || r.DependabotAlerts[1].Number != 3 {
		t.Errorf("dependabot order = %d,%d, want 1,3", r.DependabotAlerts[0].Number, r.DependabotAlerts[1].Number)
	}
	// Secret scanning: purely by ascending number (no severity).
	if r.SecretScanningAlerts[0].Number != 2 || r.SecretScanningAlerts[1].Number != 5 || r.SecretScanningAlerts[2].Number != 8 {
		t.Errorf("secret scanning order = %d,%d,%d, want 2,5,8",
			r.SecretScanningAlerts[0].Number, r.SecretScanningAlerts[1].Number, r.SecretScanningAlerts[2].Number)
	}
}

func TestSourceStatusNoteForbiddenAndError(t *testing.T) {
	forbidden := sourceStatusNote("Code scanning", model.SecuritySource{Status: model.StatusForbidden})
	if !strings.Contains(forbidden, "token lacks access") {
		t.Errorf("forbidden note = %q", forbidden)
	}
	// An unknown / error status uses the default error message.
	errNote := sourceStatusNote("Dependabot", model.SecuritySource{Status: model.SourceStatus("boom")})
	if !strings.Contains(errNote, "error —") || !strings.Contains(errNote, "could not fetch") {
		t.Errorf("error note = %q", errNote)
	}
	// A custom message overrides the fallback.
	custom := sourceStatusNote("Secret scanning", model.SecuritySource{Status: model.StatusDisabled, Message: "GHAS off"})
	if !strings.Contains(custom, "GHAS off") {
		t.Errorf("custom note = %q", custom)
	}
}

func TestLocationLabel(t *testing.T) {
	cases := []struct {
		path             string
		start, end, want int
		wantStr          string
	}{
		{path: "", wantStr: ""},
		{path: "a.go", start: 0, wantStr: "a.go"},                 // no line
		{path: "a.go", start: 10, wantStr: "a.go:10"},             // single line
		{path: "a.go", start: 10, end: 20, wantStr: "a.go:10-20"}, // range
		{path: "a.go", start: 10, end: 10, wantStr: "a.go:10"},    // end==start → single
		{path: "a.go", start: 10, end: 5, wantStr: "a.go:10"},     // end<start → single
	}
	for _, c := range cases {
		if got := locationLabel(c.path, c.start, c.end); got != c.wantStr {
			t.Errorf("locationLabel(%q,%d,%d) = %q, want %q", c.path, c.start, c.end, got, c.wantStr)
		}
	}
}

func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"   \n  \n":      "",
		"\n\nfirst real": "first real",
		"  hello  ":      "hello",
		"a\nb":           "a",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderCodeScanningDescriptionFallback(t *testing.T) {
	// With an empty Message but a Description, the alert renders the description.
	r := &model.SecurityReport{
		Owner: "o", Repo: "r", State: "open",
		CodeScanning: model.SecuritySource{Status: model.StatusOK},
		CodeScanningAlerts: []model.CodeScanningAlert{{
			Number: 1, Severity: model.SeverityMedium, RuleID: "rule/x",
			Description: "Some helpful description.", Path: "f.go", StartLine: 7,
		}},
	}
	var b bytes.Buffer
	Render(&b, r)
	out := b.String()
	if !strings.Contains(out, "Some helpful description.") {
		t.Errorf("expected description fallback, got:\n%s", out)
	}
	if !strings.Contains(out, "f.go:7") {
		t.Errorf("expected single-line location, got:\n%s", out)
	}
}

func TestRenderSecretAlertResolutionAndTypeSuffix(t *testing.T) {
	r := &model.SecurityReport{
		Owner: "o", Repo: "r", State: "all",
		SecretScanning: model.SecuritySource{Status: model.StatusOK},
		SecretScanningAlerts: []model.SecretScanningAlert{
			{
				Number: 1, State: "resolved", SecretType: "aws_key", DisplayName: "AWS Key",
				Resolution: "revoked", HTMLURL: "https://example/s/1",
				Locations: []model.SecretLocation{{Path: ".env", StartLine: 3, EndLine: 3}},
			},
			{
				// DisplayName empty → name falls back to SecretType, so the "(type)"
				// suffix is suppressed because SecretType == name.
				Number: 2, State: "open", SecretType: "slack_token",
			},
		},
	}
	var b bytes.Buffer
	Render(&b, r)
	out := b.String()
	for _, want := range []string{
		"● AWS Key (aws_key)  [resolved]",
		"resolution: revoked",
		".env:3",
		"● slack_token  [open]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	// The type suffix must not appear for the name==type case.
	if strings.Contains(out, "slack_token (slack_token)") {
		t.Errorf("type suffix should be suppressed when it equals the name:\n%s", out)
	}
}

func TestRenderStateAllNoAlerts(t *testing.T) {
	// State "all" with no alerts uses the unqualified summary + clear messages.
	r := &model.SecurityReport{
		Owner: "o", Repo: "r", State: "all",
		CodeScanning:   model.SecuritySource{Status: model.StatusOK},
		SecretScanning: model.SecuritySource{Status: model.StatusOK},
		Dependabot:     model.SecuritySource{Status: model.StatusOK},
	}
	var b bytes.Buffer
	Render(&b, r)
	out := b.String()
	if !strings.Contains(out, "Summary: no alerts") || !strings.Contains(out, "✓ No security alerts.") {
		t.Errorf("state=all clean output unexpected:\n%s", out)
	}
}
