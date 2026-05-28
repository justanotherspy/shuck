package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

// stubSecurity returns canned per-source results and counts how many times the
// network layer was hit, so caching and degradation can be tested without GitHub.
type stubSecurity struct {
	cs        []model.CodeScanningAlert
	csSrc     model.SecuritySource
	secret    []model.SecretScanningAlert
	secretSrc model.SecuritySource
	dep       []model.DependabotAlert
	depSrc    model.SecuritySource
	calls     int
	sha       string // default-branch SHA returned by DefaultBranchSHA
	shaCalls  int
	shaErr    error
}

func (s *stubSecurity) ListCodeScanningAlerts(_ context.Context, _, _, _ string) ([]model.CodeScanningAlert, model.SecuritySource) {
	s.calls++
	return s.cs, s.csSrc
}

func (s *stubSecurity) ListSecretScanningAlerts(_ context.Context, _, _, _ string) ([]model.SecretScanningAlert, model.SecuritySource) {
	return s.secret, s.secretSrc
}

func (s *stubSecurity) ListDependabotAlerts(_ context.Context, _, _, _ string) ([]model.DependabotAlert, model.SecuritySource) {
	return s.dep, s.depSrc
}

func (s *stubSecurity) DefaultBranchSHA(_ context.Context, _, _ string) (string, error) {
	s.shaCalls++
	return s.sha, s.shaErr
}

func withStubSecurity(t *testing.T, s *stubSecurity) {
	t.Helper()
	t.Setenv("SHUCK_HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	prev := newSecurityLister
	newSecurityLister = func(string) securityLister { return s }
	t.Cleanup(func() { newSecurityLister = prev })
}

func okStub() *stubSecurity {
	return &stubSecurity{
		csSrc:     model.SecuritySource{Status: model.StatusOK},
		secretSrc: model.SecuritySource{Status: model.StatusDisabled, Message: "not enabled or no access"},
		depSrc:    model.SecuritySource{Status: model.StatusOK},
		dep: []model.DependabotAlert{
			{Number: 12, State: "open", Severity: model.SeverityCritical, Ecosystem: "npm", Package: "lodash", FixedVersion: "4.17.21", GHSAID: "GHSA-jf85", Summary: "Prototype pollution"},
		},
	}
}

// TestSecurityStateCaseInsensitive proves the shared core normalizes --state so
// the `all` path and the MCP tool accept the same case-insensitive values the
// `security` subcommand does (a mixed-case or padded value must not error).
func TestSecurityStateCaseInsensitive(t *testing.T) {
	withStubSecurity(t, okStub())
	rep, err := Security(context.Background(), "o", "r", SecurityOptions{State: "  OPEN "})
	if err != nil {
		t.Fatalf("Security with mixed-case state: %v", err)
	}
	if rep.State != "open" {
		t.Errorf("state = %q, want normalized %q", rep.State, "open")
	}
}

func TestRunSecurityTextOutput(t *testing.T) {
	withStubSecurity(t, okStub())
	var out, errb bytes.Buffer
	if code := runSecurity([]string{"justanotherspy/shuck"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{
		"justanotherspy/shuck — security alerts (open)",
		"Dependabot (1):",
		"lodash → 4.17.21",
		"Secret scanning: not enabled or no access — skipped.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRunSecurityJSON(t *testing.T) {
	withStubSecurity(t, okStub())
	var out, errb bytes.Buffer
	if code := runSecurity([]string{"justanotherspy/shuck", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, `"schema_version": 1`) || !strings.Contains(got, `"total": 1`) {
		t.Errorf("unexpected JSON:\n%s", got)
	}
}

func TestRunSecurityInvalidState(t *testing.T) {
	withStubSecurity(t, okStub())
	var out, errb bytes.Buffer
	if code := runSecurity([]string{"o/r", "--state", "bogus"}, &out, &errb); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "invalid --state") {
		t.Errorf("expected invalid-state message, got %q", errb.String())
	}
}

func TestRunSecurityExitCode(t *testing.T) {
	withStubSecurity(t, okStub())
	var out, errb bytes.Buffer
	if code := runSecurity([]string{"o/r", "--exit-code"}, &out, &errb); code != 1 {
		t.Errorf("--exit-code with open alerts: exit = %d, want 1", code)
	}

	// No open alerts -> exit 0 even with --exit-code.
	withStubSecurity(t, &stubSecurity{
		csSrc:     model.SecuritySource{Status: model.StatusOK},
		secretSrc: model.SecuritySource{Status: model.StatusOK},
		depSrc:    model.SecuritySource{Status: model.StatusOK},
	})
	out.Reset()
	errb.Reset()
	if code := runSecurity([]string{"o/r", "--exit-code"}, &out, &errb); code != 0 {
		t.Errorf("--exit-code with no alerts: exit = %d, want 0", code)
	}
}

func TestRunSecurityForbiddenAndError(t *testing.T) {
	withStubSecurity(t, &stubSecurity{
		csSrc:     model.SecuritySource{Status: model.StatusForbidden, Message: "token lacks access"},
		secretSrc: model.SecuritySource{Status: model.StatusDisabled},
		depSrc:    model.SecuritySource{Status: model.StatusError, Message: "boom"},
	})
	var out, errb bytes.Buffer
	if code := runSecurity([]string{"o/r"}, &out, &errb); code != 0 {
		t.Fatalf("partial failure should still succeed: exit = %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "Code scanning: token lacks access — skipped.") {
		t.Errorf("forbidden note missing:\n%s", got)
	}
	if !strings.Contains(got, "Dependabot: error — boom") {
		t.Errorf("error note missing:\n%s", got)
	}
	// No token set and nothing OK -> the hint is printed on stderr.
	if !strings.Contains(errb.String(), "no GitHub token set") {
		t.Errorf("expected no-token hint, got %q", errb.String())
	}
}

func TestRunSecurityAllErroredIsOperationalError(t *testing.T) {
	withStubSecurity(t, &stubSecurity{
		csSrc:     model.SecuritySource{Status: model.StatusError, Message: "x"},
		secretSrc: model.SecuritySource{Status: model.StatusError, Message: "y"},
		depSrc:    model.SecuritySource{Status: model.StatusError, Message: "z"},
	})
	var out, errb bytes.Buffer
	if code := runSecurity([]string{"o/r"}, &out, &errb); code != 2 {
		t.Errorf("all sources errored: exit = %d, want 2", code)
	}
}

func TestRunSecurityCachesAcrossRuns(t *testing.T) {
	s := okStub()
	withStubSecurity(t, s)
	var out, errb bytes.Buffer
	runSecurity([]string{"o/r"}, &out, &errb)
	runSecurity([]string{"o/r"}, &out, &errb)
	if s.calls != 1 {
		t.Errorf("warm cache should avoid a second fetch: calls = %d, want 1", s.calls)
	}
	runSecurity([]string{"o/r", "--refresh"}, &out, &errb)
	if s.calls != 2 {
		t.Errorf("--refresh should re-fetch: calls = %d, want 2", s.calls)
	}
}

func TestRunSecurityRefetchesWhenDefaultSHAChanges(t *testing.T) {
	s := okStub()
	s.sha = "sha-main"
	withStubSecurity(t, s)
	var out, errb bytes.Buffer
	runSecurity([]string{"o/r"}, &out, &errb)
	s.sha = "sha-moved" // a new default-branch commit appeared
	runSecurity([]string{"o/r"}, &out, &errb)
	if s.calls != 2 {
		t.Errorf("a moved default branch should re-fetch alerts: calls = %d, want 2", s.calls)
	}
}
