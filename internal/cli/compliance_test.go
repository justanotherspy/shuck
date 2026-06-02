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
	actions     model.ActionsSettings
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

func (s *stubCompliance) ActionsSettings(_ context.Context, _, _ string) model.ActionsSettings {
	return s.actions
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

// writeConfig writes a compliance.yml into a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "compliance.yml")
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
		settings: model.RepoSettings{AllowMergeCommit: false, DeleteBranchOnMerge: true, MergeSettingsSource: model.SettingsSource{Status: model.StatusOK}},
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
		settings: model.RepoSettings{AllowMergeCommit: true, DeleteBranchOnMerge: true, MergeSettingsSource: model.SettingsSource{Status: model.StatusOK}},
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
		settings: model.RepoSettings{AllowMergeCommit: true, MergeSettingsSource: model.SettingsSource{Status: model.StatusOK}},
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
		settings: model.RepoSettings{AllowMergeCommit: false, DeleteBranchOnMerge: true, MergeSettingsSource: model.SettingsSource{Status: model.StatusOK}},
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
		fileErr: errors.New("get .github/compliance.yml from o/r: GET https://api.github.com/...: 404 Not Found"),
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

// discoverStub returns a stub with fully readable live settings, suitable for
// exercising `shuck compliance discover`.
func discoverStub() *stubCompliance {
	return &stubCompliance{
		settings: model.RepoSettings{
			Visibility:                   "public",
			DefaultBranch:                "main",
			AllowSquashMerge:             true,
			DeleteBranchOnMerge:          true,
			SecretScanning:               "enabled",
			SecretScanningPushProtection: "enabled",
			DependabotSecurityUpdates:    "enabled",
			SecuritySource:               model.SettingsSource{Status: model.StatusOK},
			MergeSettingsSource:          model.SettingsSource{Status: model.StatusOK},
		},
		vuln:    true,
		vulnSrc: model.SettingsSource{Status: model.StatusOK},
		actions: model.ActionsSettings{
			Enabled:                      true,
			AllowedActions:               "all",
			PermissionsSource:            model.SettingsSource{Status: model.StatusOK},
			DefaultWorkflowPermissions:   "read",
			CanApprovePullRequestReviews: false,
			WorkflowPermissionsSource:    model.SettingsSource{Status: model.StatusOK},
			ForkPRContributorApproval:    "first_time_contributors",
			ForkPRApprovalSource:         model.SettingsSource{Status: model.StatusOK},
		},
		branches: map[string]struct {
			bp  model.BranchProtection
			src model.SettingsSource
		}{
			"main": {
				bp: model.BranchProtection{
					Branch: "main", Protected: true,
					RequiredPullRequestReviews:   true,
					RequiredApprovingReviewCount: 1,
					EnforceAdmins:                true,
				},
				src: model.SettingsSource{Status: model.StatusOK},
			},
		},
	}
}

func TestRunComplianceDiscoverCreates(t *testing.T) {
	withStubCompliance(t, discoverStub())
	path := filepath.Join(t.TempDir(), ".github", "compliance.yml")

	var out, errb bytes.Buffer
	code := runCompliance([]string{"discover", "o/r", "--config", path}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	for _, want := range []string{
		"visibility: public",
		"allow_squash_merge: true",
		"secret_scanning: true",
		"vulnerability_alerts: true",
		"required_approving_review_count: 1",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %q in written config:\n%s", want, data)
		}
	}
	got := out.String()
	for _, want := range []string{"o/r — compliance discover", "Created " + path} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestRunComplianceDiscoverDefaultPathInLocalRepo(t *testing.T) {
	withStubCompliance(t, discoverStub())
	dir := t.TempDir()
	t.Chdir(dir)

	var out, errb bytes.Buffer
	// Repo is given explicitly so target resolution does not need a git checkout;
	// the config is still written to the local default path.
	code := runCompliance([]string{"discover", "o/r"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".github", "compliance.yml")); err != nil {
		t.Fatalf("default config not created: %v", err)
	}
}

func TestRunComplianceDiscoverUpdatesDrift(t *testing.T) {
	withStubCompliance(t, discoverStub())
	path := writeConfig(t, "# keep this comment\nrepository:\n  allow_squash_merge: false\n  visibility: public\n")

	var out, errb bytes.Buffer
	code := runCompliance([]string{"discover", "o/r", "--config", path}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "# keep this comment") {
		t.Errorf("comment lost on update:\n%s", got)
	}
	if !strings.Contains(got, "allow_squash_merge: true") {
		t.Errorf("drifted value not synced:\n%s", got)
	}
	if strings.Contains(got, "secret_scanning") {
		t.Errorf("update must not add undeclared keys:\n%s", got)
	}
	if !strings.Contains(out.String(), "~ repository.allow_squash_merge: false → true") {
		t.Errorf("missing change line in output:\n%s", out.String())
	}
}

func TestRunComplianceDiscoverNoDrift(t *testing.T) {
	withStubCompliance(t, discoverStub())
	body := "repository:\n  allow_squash_merge: true\n"
	path := writeConfig(t, body)

	var out, errb bytes.Buffer
	code := runCompliance([]string{"discover", "o/r", "--config", path}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != body {
		t.Errorf("an up-to-date config must not be rewritten:\n%s", data)
	}
	if !strings.Contains(out.String(), "already matches the live settings") {
		t.Errorf("missing up-to-date message:\n%s", out.String())
	}
}

func TestRunComplianceDiscoverDryRun(t *testing.T) {
	withStubCompliance(t, discoverStub())
	path := filepath.Join(t.TempDir(), "compliance.yml")

	var out, errb bytes.Buffer
	code := runCompliance([]string{"discover", "o/r", "--config", path, "--dry-run"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("--dry-run must not write the config (stat err = %v)", err)
	}
	got := out.String()
	if !strings.Contains(got, "Would create") || !strings.Contains(got, "visibility: public") {
		t.Errorf("dry run should preview the config:\n%s", got)
	}
}

func TestRunComplianceDiscoverJSON(t *testing.T) {
	withStubCompliance(t, discoverStub())
	path := filepath.Join(t.TempDir(), "compliance.yml")

	var out, errb bytes.Buffer
	code := runCompliance([]string{"discover", "o/r", "--config", path, "--json"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{`"schema_version": 1`, `"created": true`, `"up_to_date": false`, `"path": `} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in JSON:\n%s", want, got)
		}
	}
}

func TestRunComplianceDiscoverInvalidExisting(t *testing.T) {
	withStubCompliance(t, discoverStub())
	path := writeConfig(t, "repository:\n  allow_squash_merg: true\n") // typo'd key

	var out, errb bytes.Buffer
	code := runCompliance([]string{"discover", "o/r", "--config", path}, &out, &errb)
	if code != 2 {
		t.Fatalf("a typo'd existing config should be an operational error (2), got %d", code)
	}
}

func TestRunComplianceDiscoverRepoUnreadable(t *testing.T) {
	withStubCompliance(t, &stubCompliance{settingsErr: context.DeadlineExceeded})
	path := filepath.Join(t.TempDir(), "compliance.yml")

	var out, errb bytes.Buffer
	code := runCompliance([]string{"discover", "o/r", "--config", path}, &out, &errb)
	if code != 2 {
		t.Fatalf("an unreadable repo should exit 2, got %d", code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("no config should be written on error")
	}
}

func TestRunComplianceActions(t *testing.T) {
	withStubCompliance(t, &stubCompliance{
		actions: model.ActionsSettings{
			Enabled:                      true,
			AllowedActions:               "all",
			PermissionsSource:            model.SettingsSource{Status: model.StatusOK},
			DefaultWorkflowPermissions:   "write", // drift from the declared "read"
			CanApprovePullRequestReviews: false,
			WorkflowPermissionsSource:    model.SettingsSource{Status: model.StatusOK},
			// Fork approval not readable: skipped, not failed.
			ForkPRApprovalSource: model.SettingsSource{Status: model.StatusForbidden, Message: "needs admin"},
		},
	})
	cfg := writeConfig(t, `actions:
  enabled: true
  default_workflow_permissions: read
  can_approve_pull_request_reviews: false
  fork_pr_contributor_approval: first_time_contributors
`)
	var out, errb bytes.Buffer
	code := runCompliance([]string{"o/r", "--config", cfg}, &out, &errb)
	if code != 1 {
		t.Fatalf("actions drift should exit 1, got %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{
		"Actions:",
		"✓ enabled = true",
		"✗ default_workflow_permissions: want read, got write",
		"✓ can_approve_pull_request_reviews = false",
		"– fork_pr_contributor_approval: want first_time_contributors — skipped (needs admin)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
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
