package compliance

import (
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestParse(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg, err := Parse([]byte(`
repository:
  visibility: private
  allow_merge_commit: false
security:
  secret_scanning: true
branch_protection:
  main:
    required_approving_review_count: 2
    required_status_checks:
      - build
      - test
`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.Repository == nil || cfg.Repository.Visibility == nil || *cfg.Repository.Visibility != "private" {
			t.Errorf("visibility not parsed: %+v", cfg.Repository)
		}
		if cfg.Security == nil || cfg.Security.SecretScanning == nil || !*cfg.Security.SecretScanning {
			t.Errorf("secret_scanning not parsed: %+v", cfg.Security)
		}
		bc := cfg.BranchProtection["main"]
		if bc == nil || bc.RequiredApprovingReviewCount == nil || *bc.RequiredApprovingReviewCount != 2 {
			t.Fatalf("branch config not parsed: %+v", bc)
		}
		if bc.RequiredStatusChecks == nil || len(*bc.RequiredStatusChecks) != 2 {
			t.Errorf("status checks not parsed: %+v", bc.RequiredStatusChecks)
		}
	})

	t.Run("unknown field rejected", func(t *testing.T) {
		_, err := Parse([]byte("repository:\n  allow_merg_commit: false\n"))
		if err == nil {
			t.Fatal("expected an error for an unknown key (typo)")
		}
	})

	t.Run("empty rejected", func(t *testing.T) {
		if _, err := Parse([]byte("{}\n")); err == nil {
			t.Fatal("expected an error for an empty config")
		}
	})

	t.Run("invalid visibility", func(t *testing.T) {
		if _, err := Parse([]byte("repository:\n  visibility: secret\n")); err == nil {
			t.Fatal("expected an error for an invalid visibility")
		}
	})
}

func TestEvaluateRepository(t *testing.T) {
	cfg := Config{Repository: &RepositoryConfig{
		AllowMergeCommit:    new(false),
		DeleteBranchOnMerge: new(true),
		DefaultBranch:       new("main"),
	}}
	actual := Actual{Settings: model.RepoSettings{
		AllowMergeCommit:    true, // drift
		DeleteBranchOnMerge: true, // match
		DefaultBranch:       "main",
	}}
	rep := Evaluate("o", "r", "cfg", cfg, actual)
	if len(rep.Checks) != 3 {
		t.Fatalf("want 3 checks, got %d", len(rep.Checks))
	}
	if got := rep.Count(model.ComplianceFail); got != 1 {
		t.Errorf("want 1 fail, got %d", got)
	}
	if !rep.HasFailures() || rep.Compliant() {
		t.Errorf("report should be non-compliant")
	}
	// Find the merge-commit check and confirm expected/actual.
	var found bool
	for _, c := range rep.Checks {
		if c.Setting == "allow_merge_commit" {
			found = true
			if c.Status != model.ComplianceFail || c.Expected != "false" || c.Actual != "true" {
				t.Errorf("merge check wrong: %+v", c)
			}
		}
	}
	if !found {
		t.Error("allow_merge_commit check missing")
	}
}

func TestEvaluateSecuritySkippedWhenForbidden(t *testing.T) {
	cfg := Config{Security: &SecurityConfig{
		SecretScanning:      new(true),
		VulnerabilityAlerts: new(true),
	}}
	actual := Actual{
		Settings: model.RepoSettings{
			// Security section not readable (non-admin token).
			SecuritySource: model.SettingsSource{Status: model.StatusForbidden, Message: "needs admin"},
		},
		VulnAlerts: true,
		VulnSource: model.SettingsSource{Status: model.StatusOK},
	}
	rep := Evaluate("o", "r", "cfg", cfg, actual)
	if got := rep.Count(model.ComplianceSkipped); got != 1 {
		t.Errorf("secret_scanning should be skipped: %d skipped", got)
	}
	if got := rep.Count(model.CompliancePass); got != 1 {
		t.Errorf("vulnerability_alerts should pass: %d pass", got)
	}
	if rep.HasFailures() {
		t.Errorf("skipped + pass should not be a failure")
	}
}

func TestEvaluateSecurityEnabledMapping(t *testing.T) {
	cfg := Config{Security: &SecurityConfig{SecretScanning: new(true)}}
	actual := Actual{Settings: model.RepoSettings{
		SecretScanning: "enabled",
		SecuritySource: model.SettingsSource{Status: model.StatusOK},
	}}
	rep := Evaluate("o", "r", "cfg", cfg, actual)
	if !rep.Compliant() {
		t.Errorf("enabled secret scanning should match a true config: %+v", rep.Checks)
	}
	if rep.Checks[0].Expected != "enabled" || rep.Checks[0].Actual != "enabled" {
		t.Errorf("enabled mapping wrong: %+v", rep.Checks[0])
	}
}

func TestEvaluateBranchProtection(t *testing.T) {
	cfg := Config{BranchProtection: map[string]*BranchConfig{
		"main": {
			RequiredApprovingReviewCount: new(2),
			EnforceAdmins:                new(true),
			RequiredStatusChecks:         &[]string{"build", "test"},
		},
	}}

	t.Run("protected match", func(t *testing.T) {
		actual := Actual{Branches: map[string]Branch{
			"main": {
				Source: model.SettingsSource{Status: model.StatusOK},
				Protection: model.BranchProtection{
					Protected:                    true,
					RequiredApprovingReviewCount: 2,
					EnforceAdmins:                true,
					RequiredStatusChecks:         []string{"test", "build"}, // order-insensitive
				},
			},
		}}
		rep := Evaluate("o", "r", "cfg", cfg, actual)
		if !rep.Compliant() {
			t.Errorf("should be compliant: %+v", rep.Checks)
		}
	})

	t.Run("unprotected fails all", func(t *testing.T) {
		actual := Actual{Branches: map[string]Branch{
			"main": {
				Source:     model.SettingsSource{Status: model.StatusOK},
				Protection: model.BranchProtection{Protected: false},
			},
		}}
		rep := Evaluate("o", "r", "cfg", cfg, actual)
		if rep.Count(model.ComplianceFail) != 3 {
			t.Errorf("unprotected branch should fail all 3 declared keys: %+v", rep.Checks)
		}
		for _, c := range rep.Checks {
			if !strings.Contains(c.Actual, "not protected") {
				t.Errorf("expected 'not protected' actual: %+v", c)
			}
		}
	})

	t.Run("forbidden skips all", func(t *testing.T) {
		actual := Actual{Branches: map[string]Branch{
			"main": {
				Source: model.SettingsSource{Status: model.StatusForbidden, Message: "no access"},
			},
		}}
		rep := Evaluate("o", "r", "cfg", cfg, actual)
		if rep.Count(model.ComplianceSkipped) != 3 {
			t.Errorf("forbidden branch should skip all 3 keys: %+v", rep.Checks)
		}
		if rep.HasFailures() {
			t.Errorf("forbidden should not be a failure")
		}
	})

	t.Run("status check drift", func(t *testing.T) {
		actual := Actual{Branches: map[string]Branch{
			"main": {
				Source: model.SettingsSource{Status: model.StatusOK},
				Protection: model.BranchProtection{
					Protected:                    true,
					RequiredApprovingReviewCount: 2,
					EnforceAdmins:                true,
					RequiredStatusChecks:         []string{"build"}, // missing "test"
				},
			},
		}}
		rep := Evaluate("o", "r", "cfg", cfg, actual)
		if rep.Count(model.ComplianceFail) != 1 {
			t.Errorf("want 1 status-check failure: %+v", rep.Checks)
		}
	})
}

func TestEvaluateOrderingStable(t *testing.T) {
	cfg := Config{BranchProtection: map[string]*BranchConfig{
		"release": {EnforceAdmins: new(true)},
		"main":    {EnforceAdmins: new(true)},
	}}
	actual := Actual{Branches: map[string]Branch{
		"release": {Source: model.SettingsSource{Status: model.StatusOK}, Protection: model.BranchProtection{Protected: true, EnforceAdmins: true}},
		"main":    {Source: model.SettingsSource{Status: model.StatusOK}, Protection: model.BranchProtection{Protected: true, EnforceAdmins: true}},
	}}
	rep := Evaluate("o", "r", "cfg", cfg, actual)
	if rep.Checks[0].Setting != "main.enforce_admins" || rep.Checks[1].Setting != "release.enforce_admins" {
		t.Errorf("branches should be sorted: %+v", rep.Checks)
	}
}
