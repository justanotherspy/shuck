package gh

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v89/github"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestNormalizeSeverity(t *testing.T) {
	tests := map[string]model.SecuritySeverity{
		"critical":  model.SeverityCritical,
		"CRITICAL":  model.SeverityCritical,
		"high":      model.SeverityHigh,
		"error":     model.SeverityHigh,
		"medium":    model.SeverityMedium,
		"moderate":  model.SeverityMedium,
		"low":       model.SeverityLow,
		"warning":   model.SeverityWarning,
		"note":      model.SeverityNote,
		"  high  ":  model.SeverityHigh, // trimmed
		"":          model.SeverityUnknown,
		"gibberish": model.SeverityUnknown,
	}
	for in, want := range tests {
		if got := normalizeSeverity(in); got != want {
			t.Errorf("normalizeSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassifySecurityErr(t *testing.T) {
	mk := func(code int) error {
		return &github.ErrorResponse{Response: &http.Response{StatusCode: code}}
	}
	tests := []struct {
		name     string
		err      error
		wantSoft bool
		wantStat model.SourceStatus
	}{
		{"404 disabled", mk(http.StatusNotFound), true, model.StatusDisabled},
		{"403 forbidden", mk(http.StatusForbidden), true, model.StatusForbidden},
		{"500 hard", mk(http.StatusInternalServerError), false, ""},
		{"plain error", errors.New("boom"), false, ""},
		{"github err no response", &github.ErrorResponse{}, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src, soft := classifySecurityErr(tc.err)
			if soft != tc.wantSoft {
				t.Fatalf("soft = %v, want %v", soft, tc.wantSoft)
			}
			if soft && src.Status != tc.wantStat {
				t.Errorf("status = %q, want %q", src.Status, tc.wantStat)
			}
		})
	}
}

func TestStateNotApplicable(t *testing.T) {
	src := stateNotApplicable("fixed")
	if src.Status != model.StatusDisabled {
		t.Errorf("status = %q, want disabled", src.Status)
	}
	if src.Message == "" {
		t.Error("expected an explanatory message")
	}
}

func TestStateMappers(t *testing.T) {
	type want struct {
		api string
		ok  bool
	}
	cs := map[string]want{
		"open": {"open", true}, "all": {"", true}, "dismissed": {"dismissed", true},
		"fixed": {"fixed", true}, "resolved": {"", false},
	}
	for in, w := range cs {
		if a, ok := codeScanningState(in); a != w.api || ok != w.ok {
			t.Errorf("codeScanningState(%q) = (%q,%v), want (%q,%v)", in, a, ok, w.api, w.ok)
		}
	}
	sc := map[string]want{
		"open": {"open", true}, "all": {"", true}, "resolved": {"resolved", true},
		"dismissed": {"", false}, "fixed": {"", false},
	}
	for in, w := range sc {
		if a, ok := secretScanningState(in); a != w.api || ok != w.ok {
			t.Errorf("secretScanningState(%q) = (%q,%v), want (%q,%v)", in, a, ok, w.api, w.ok)
		}
	}
	db := map[string]want{
		"open": {"open", true}, "all": {"", true}, "dismissed": {"dismissed", true},
		"fixed": {"fixed", true}, "resolved": {"", false},
	}
	for in, w := range db {
		if a, ok := dependabotState(in); a != w.api || ok != w.ok {
			t.Errorf("dependabotState(%q) = (%q,%v), want (%q,%v)", in, a, ok, w.api, w.ok)
		}
	}
}

func TestMapCodeScanningAlert(t *testing.T) {
	a := &github.Alert{
		Number:          new(3),
		State:           new("open"),
		RuleID:          new("go/sql-injection"),
		RuleDescription: new("SQL injection"),
		Rule: &github.Rule{
			SecuritySeverityLevel: new("high"),
		},
		Tool: &github.Tool{Name: new("CodeQL")},
		MostRecentInstance: &github.MostRecentInstance{
			Location: &github.Location{Path: new("db.go"), StartLine: new(10), EndLine: new(12)},
			Message:  &github.Message{Text: new("tainted input")},
		},
		HTMLURL: new("https://example/alert/3"),
	}
	got := mapCodeScanningAlert(a)
	if got.Number != 3 || got.Severity != model.SeverityHigh || got.RuleID != "go/sql-injection" {
		t.Errorf("alert = %+v", got)
	}
	if got.Path != "db.go" || got.StartLine != 10 || got.Message != "tainted input" {
		t.Errorf("location = %+v", got)
	}

	// When the security-severity level is absent, fall back to the rule severity.
	b := &github.Alert{RuleSeverity: new("warning")}
	if mapCodeScanningAlert(b).Severity != model.SeverityWarning {
		t.Errorf("fallback severity = %q", mapCodeScanningAlert(b).Severity)
	}
}

func TestMapSecretScanningAlert(t *testing.T) {
	a := &github.SecretScanningAlert{
		Number:                new(5),
		State:                 new("open"),
		SecretType:            new("aws_access_key"),
		SecretTypeDisplayName: new("AWS Access Key"),
		Resolution:            new(""),
		HTMLURL:               new("https://example/secret/5"),
	}
	got := mapSecretScanningAlert(a)
	if got.Number != 5 || got.SecretType != "aws_access_key" || got.DisplayName != "AWS Access Key" {
		t.Errorf("alert = %+v", got)
	}
}

func TestMapDependabotAlert(t *testing.T) {
	a := &github.DependabotAlert{
		Number: new(8),
		State:  new("open"),
		Dependency: &github.Dependency{
			Package:      &github.VulnerabilityPackage{Ecosystem: new("npm"), Name: new("left-pad")},
			ManifestPath: new("package.json"),
		},
		SecurityAdvisory: &github.DependabotSecurityAdvisory{
			Severity: new("critical"),
			GHSAID:   new("GHSA-xxxx"),
			CVEID:    new("CVE-2024-0001"),
			Summary:  new("malware"),
		},
		SecurityVulnerability: &github.AdvisoryVulnerability{
			VulnerableVersionRange: new("< 1.0.0"),
			FirstPatchedVersion:    &github.FirstPatchedVersion{Identifier: new("1.0.0")},
		},
		HTMLURL: new("https://example/dep/8"),
	}
	got := mapDependabotAlert(a)
	if got.Number != 8 || got.Severity != model.SeverityCritical || got.Package != "left-pad" {
		t.Errorf("alert = %+v", got)
	}
	if got.Ecosystem != "npm" || got.FixedVersion != "1.0.0" || got.GHSAID != "GHSA-xxxx" {
		t.Errorf("detail = %+v", got)
	}
}

func TestListCodeScanningAlerts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/code-scanning/alerts" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[
			{"number":1,"state":"open","rule":{"id":"x","security_severity_level":"high"}}
		]`))
	}))
	defer srv.Close()

	alerts, src := testClient(t, srv).ListCodeScanningAlerts(context.Background(), "o", "r", "open")
	if src.Status != model.StatusOK {
		t.Fatalf("status = %q (%s)", src.Status, src.Message)
	}
	if len(alerts) != 1 || alerts[0].Number != 1 {
		t.Errorf("alerts = %+v", alerts)
	}
}

func TestListCodeScanningAlertsStateNA(t *testing.T) {
	// "resolved" has no code-scanning equivalent: it short-circuits to disabled
	// without any network call.
	c := New("")
	alerts, src := c.ListCodeScanningAlerts(context.Background(), "o", "r", "resolved")
	if alerts != nil || src.Status != model.StatusDisabled {
		t.Errorf("alerts=%+v src=%+v, want nil/disabled", alerts, src)
	}
}

func TestListCodeScanningAlertsDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	alerts, src := testClient(t, srv).ListCodeScanningAlerts(context.Background(), "o", "r", "open")
	if alerts != nil || src.Status != model.StatusDisabled {
		t.Errorf("alerts=%+v src=%+v, want nil/disabled", alerts, src)
	}
}

func TestListCodeScanningAlertsHardError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, src := testClient(t, srv).ListCodeScanningAlerts(context.Background(), "o", "r", "open")
	if src.Status != model.StatusError {
		t.Errorf("status = %q, want error", src.Status)
	}
}

func TestListSecretScanningAlerts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/secret-scanning/alerts":
			_, _ = w.Write([]byte(`[
				{"number":4,"state":"open","secret_type":"token","secret_type_display_name":"Token"}
			]`))
		case "/repos/o/r/secret-scanning/alerts/4/locations":
			// One file location is kept, one non-file (nil details) is skipped.
			_, _ = w.Write([]byte(`[
				{"type":"commit","details":{"path":"config.yml","start_line":1,"end_line":1}},
				{"type":"issue_title"}
			]`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	alerts, src := testClient(t, srv).ListSecretScanningAlerts(context.Background(), "o", "r", "open")
	if src.Status != model.StatusOK {
		t.Fatalf("status = %q (%s)", src.Status, src.Message)
	}
	if len(alerts) != 1 || len(alerts[0].Locations) != 1 || alerts[0].Locations[0].Path != "config.yml" {
		t.Errorf("alerts = %+v", alerts)
	}
}

func TestListSecretScanningAlertsStateNA(t *testing.T) {
	_, src := New("").ListSecretScanningAlerts(context.Background(), "o", "r", "fixed")
	if src.Status != model.StatusDisabled {
		t.Errorf("status = %q, want disabled", src.Status)
	}
}

func TestSecretLocationsError(t *testing.T) {
	// A location-listing error is best-effort: it returns whatever was gathered
	// (nothing here) without failing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	locs := testClient(t, srv).secretLocations(context.Background(), "o", "r", 1)
	if locs != nil {
		t.Errorf("locs = %+v, want nil on error", locs)
	}
}

func TestListDependabotAlerts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/dependabot/alerts" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("state"); got != "open" {
			t.Errorf("state filter = %q, want open", got)
		}
		_, _ = w.Write([]byte(`[
			{"number":2,"state":"open","security_advisory":{"severity":"high"}}
		]`))
	}))
	defer srv.Close()

	alerts, src := testClient(t, srv).ListDependabotAlerts(context.Background(), "o", "r", "open")
	if src.Status != model.StatusOK {
		t.Fatalf("status = %q (%s)", src.Status, src.Message)
	}
	if len(alerts) != 1 || alerts[0].Severity != model.SeverityHigh {
		t.Errorf("alerts = %+v", alerts)
	}
}

func TestListDependabotAlertsStateNA(t *testing.T) {
	_, src := New("").ListDependabotAlerts(context.Background(), "o", "r", "resolved")
	if src.Status != model.StatusDisabled {
		t.Errorf("status = %q, want disabled", src.Status)
	}
}

func TestListDependabotAlertsForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	_, src := testClient(t, srv).ListDependabotAlerts(context.Background(), "o", "r", "open")
	if src.Status != model.StatusForbidden {
		t.Errorf("status = %q, want forbidden", src.Status)
	}
}
