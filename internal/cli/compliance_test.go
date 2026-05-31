package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

// stubCompliance returns canned live settings so the command can be exercised
// without GitHub. file, when set, is returned by FileContent (else a 404).
type stubCompliance struct {
	settings    model.RepoSettings
	settingsErr error
	vuln        bool
	vulnSrc     model.SettingsSource
	branches    map[string]struct {
		bp  model.BranchProtection
		src model.SettingsSource
	}
	file    []byte
	fileErr error
}

func (s *stubCompliance) RepoSettings(_ context.Context, _, _ string) (model.RepoSettings, error) {
	return s.settings, s.settingsErr
}

func (s *stubCompliance) VulnerabilityAlertsEnabled(_ context.Context, _, _ string) (bool, model.SettingsSource) {
	return s.vuln, s.vulnSrc
}

func (s *stubCompliance) BranchProtectionSettings(_ context.Context, _, _, branch string) (model.BranchProtection, model.SettingsSource) {
	if b, ok := s.branches[branch]; ok {
		return b.bp, b.src
	}
	return model.BranchProtection{Branch: branch, Protected: false}, model.SettingsSource{Status: model.StatusOK}
}

func (s *stubCompliance) FileContent(_ context.Context, _, _, _, _ string) ([]byte, error) {
	if s.fileErr != nil {
		return nil, s.fileErr
	}
	return s.file, nil
}

func withStubCompliance(t *testing.T, s *stubCompliance) {
	t.Helper()
	t.Setenv("SHUCK_HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	prev := newComplianceLister
	newComplianceLister = func(string) complianceLister { return s }
	t.Cleanup(func() { newComplianceLister = prev })
}

// writeConfig writes a compliance.yaml into a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "compliance.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const okConfig = `repository:
  allow_merge_commit: false
  delete_branch_on_merge: true
`

func TestRunComplianceTextPass(t *testing.T) {
	withStubCompliance(t, &stubCompliance{
		settings: model.RepoSettings{AllowMergeCommit: false, DeleteBranchOnMerge: true},
	})
	cfg := writeConfig(t, okConfig)
	var out, errb bytes.Buffer
	code := runCompliance([]string{"o/r", "--config", cfg}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{
		"o/r — compliance",
		"✓ allow_merge_commit = false",
		"✓ Compliant",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRunComplianceDriftExits1(t *testing.T) {
	withStubCompliance(t, &stubCompliance{
		settings: model.RepoSettings{AllowMergeCommit: true, DeleteBranchOnMerge: true},
	})
	cfg := writeConfig(t, okConfig)
	var out, errb bytes.Buffer
	code := runCompliance([]string{"o/r", "--config", cfg}, &out, &errb)
	if code != 1 {
		t.Fatalf("drift should exit 1, got %d", code)
	}
	if !strings.Contains(out.String(), "✗ allow_merge_commit: want false, got true") {
		t.Errorf("missing drift line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Not compliant") {
		t.Errorf("missing non-compliant footer:\n%s", out.String())
	}
}

func TestRunComplianceExitZero(t *testing.T) {
	withStubCompliance(t, &stubCompliance{
		settings: model.RepoSettings{AllowMergeCommit: true},
	})
	cfg := writeConfig(t, "repository:\n  allow_merge_commit: false\n")
	var out, errb bytes.Buffer
	code := runCompliance([]string{"o/r", "--config", cfg, "--exit-zero"}, &out, &errb)
	if code != 0 {
		t.Fatalf("--exit-zero should exit 0 despite drift, got %d", code)
	}
}

func TestRunComplianceJSON(t *testing.T) {
	withStubCompliance(t, &stubCompliance{
		settings: model.RepoSettings{AllowMergeCommit: false, DeleteBranchOnMerge: true},
	})
	cfg := writeConfig(t, okConfig)
	var out, errb bytes.Buffer
	if code := runCompliance([]string{"o/r", "--config", cfg, "--json"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{`"schema_version": 1`, `"compliant": true`, `"total": 2`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in JSON:\n%s", want, got)
		}
	}
}

func TestRunComplianceConfigNotFound(t *testing.T) {
	withStubCompliance(t, &stubCompliance{
		// gh.FileNotFound falls back to a "404" substring check when the error is
		// not a typed *github.ErrorResponse, which is what a 404 wraps to.
		fileErr: errors.New("get .shuck/compliance.yaml from o/r: GET https://api.github.com/...: 404 Not Found"),
	})
	var out, errb bytes.Buffer
	code := runCompliance([]string{"o/r"}, &out, &errb)
	if code != 2 {
		t.Fatalf("missing config should be an operational error (2), got %d", code)
	}
	if !strings.Contains(errb.String(), "no compliance config found") {
		t.Errorf("expected not-found guidance, got %q", errb.String())
	}
}

func TestRunComplianceRepoUnreadable(t *testing.T) {
	withStubCompliance(t, &stubCompliance{
		settingsErr: context.DeadlineExceeded,
		file:        []byte(okConfig),
	})
	var out, errb bytes.Buffer
	// No --config and an explicit repo -> config is fetched (stub returns file),
	// then RepoSettings fails -> operational error.
	code := runCompliance([]string{"o/r"}, &out, &errb)
	if code != 2 {
		t.Fatalf("unreadable repo should exit 2, got %d", code)
	}
}

func TestRunComplianceSecuritySkipped(t *testing.T) {
	withStubCompliance(t, &stubCompliance{
		settings: model.RepoSettings{
			SecuritySource: model.SettingsSource{Status: model.StatusForbidden, Message: "needs admin"},
		},
	})
	cfg := writeConfig(t, "security:\n  secret_scanning: true\n")
	var out, errb bytes.Buffer
	code := runCompliance([]string{"o/r", "--config", cfg}, &out, &errb)
	if code != 0 {
		t.Fatalf("a skipped check is not a failure, exit = %d", code)
	}
	if !strings.Contains(out.String(), "skipped (needs admin)") {
		t.Errorf("expected skipped note:\n%s", out.String())
	}
}
