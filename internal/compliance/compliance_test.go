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

	t.Run("actions section", func(t *testing.T) {
		cfg, err := Parse([]byte(`
actions:
  enabled: true
  allowed_actions: selected
  sha_pinning_required: true
  default_workflow_permissions: read
  can_approve_pull_request_reviews: false
  fork_pr_contributor_approval: all_external_contributors
`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		a := cfg.Actions
		if a == nil || a.Enabled == nil || !*a.Enabled {
			t.Fatalf("actions.enabled not parsed: %+v", a)
		}
		if a.AllowedActions == nil || *a.AllowedActions != "selected" {
			t.Errorf("allowed_actions not parsed: %+v", a)
		}
		if a.DefaultWorkflowPermissions == nil || *a.DefaultWorkflowPermissions != "read" {
			t.Errorf("default_workflow_permissions not parsed: %+v", a)
		}
	})

	t.Run("actions-only config is not empty", func(t *testing.T) {
		if _, err := Parse([]byte("actions:\n  default_workflow_permissions: read\n")); err != nil {
			t.Fatalf("an actions-only config should be valid: %v", err)
		}
	})

	t.Run("invalid enums rejected", func(t *testing.T) {
		for _, body := range []string{
			"actions:\n  allowed_actions: everything\n",
			"actions:\n  default_workflow_permissions: admin\n",
			"actions:\n  fork_pr_contributor_approval: nobody\n",
			"repository:\n  squash_merge_commit_title: TITLE\n",
			"repository:\n  squash_merge_commit_message: BODY\n",
			"repository:\n  merge_commit_title: TITLE\n",
			"repository:\n  merge_commit_message: BODY\n",
		} {
			if _, err := Parse([]byte(body)); err == nil {
				t.Errorf("expected an enum error for:\n%s", body)
			}
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
		MergeSettingsSource: model.SettingsSource{Status: model.StatusOK},
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

func TestEvaluateMergeSettingsSkippedWhenUnreadable(t *testing.T) {
	cfg := Config{Repository: &RepositoryConfig{
		AllowSquashMerge:    new(true),
		DeleteBranchOnMerge: new(true),
		HasWiki:             new(false),
	}}
	actual := Actual{Settings: model.RepoSettings{
		// Fine-grained / app token: the merge fields were absent from the API
		// response, so their (false) values must not be compared.
		MergeSettingsSource: model.SettingsSource{Status: model.StatusForbidden, Message: "needs a classic PAT"},
	}}
	rep := Evaluate("o", "r", "cfg", cfg, actual)
	if got := rep.Count(model.ComplianceSkipped); got != 2 {
		t.Errorf("unreadable merge settings should be skipped, got %d skipped: %+v", got, rep.Checks)
	}
	if got := rep.Count(model.CompliancePass); got != 1 {
		t.Errorf("has_wiki should still pass: %+v", rep.Checks)
	}
	if rep.HasFailures() {
		t.Error("unreadable merge settings must not be a failure")
	}
}

func TestEvaluateRulesetOnlyBranch(t *testing.T) {
	cfg := Config{BranchProtection: map[string]*BranchConfig{
		"main": {
			RequiredApprovingReviewCount: new(1),
			EnforceAdmins:                new(true),
			AllowForcePushes:             new(false),
		},
	}}
	actual := Actual{Branches: map[string]Branch{
		"main": {
			Source: model.SettingsSource{Status: model.StatusOK},
			Protection: model.BranchProtection{
				Branch: "main", Protected: true, ViaRulesetsOnly: true,
				RequiredPullRequestReviews:   true,
				RequiredApprovingReviewCount: 1,
				AllowForcePushes:             false,
			},
		},
	}}
	rep := Evaluate("o", "r", "cfg", cfg, actual)
	if got := rep.Count(model.CompliancePass); got != 2 {
		t.Errorf("ruleset protections should pass, got %d pass: %+v", got, rep.Checks)
	}
	// enforce_admins has no ruleset equivalent: skipped, not failed as false.
	if got := rep.Count(model.ComplianceSkipped); got != 1 {
		t.Errorf("enforce_admins should be skipped for ruleset-only protection: %+v", rep.Checks)
	}
	if rep.HasFailures() {
		t.Errorf("report should have no failures: %+v", rep.Checks)
	}
}

func TestEvaluateActions(t *testing.T) {
	cfg := Config{Actions: &ActionsConfig{
		Enabled:                      new(true),
		AllowedActions:               new("all"),
		SHAPinningRequired:           new(true),
		DefaultWorkflowPermissions:   new("read"),
		CanApprovePullRequestReviews: new(false),
		ForkPRContributorApproval:    new("first_time_contributors"),
	}}

	t.Run("readable with drift", func(t *testing.T) {
		actual := Actual{Actions: model.ActionsSettings{
			Enabled:                      true,
			AllowedActions:               "all",
			SHAPinningRequired:           false, // drift
			PermissionsSource:            model.SettingsSource{Status: model.StatusOK},
			DefaultWorkflowPermissions:   "write", // drift
			CanApprovePullRequestReviews: false,
			WorkflowPermissionsSource:    model.SettingsSource{Status: model.StatusOK},
			ForkPRContributorApproval:    "first_time_contributors",
			ForkPRApprovalSource:         model.SettingsSource{Status: model.StatusOK},
		}}
		rep := Evaluate("o", "r", "cfg", cfg, actual)
		if len(rep.Checks) != 6 {
			t.Fatalf("want 6 actions checks, got %d: %+v", len(rep.Checks), rep.Checks)
		}
		if got := rep.Count(model.ComplianceFail); got != 2 {
			t.Errorf("want 2 fails (sha pinning + workflow permissions), got %d: %+v", got, rep.Checks)
		}
		for _, c := range rep.Checks {
			if c.Category != "actions" {
				t.Errorf("check should be in the actions category: %+v", c)
			}
		}
	})

	t.Run("groups skip independently", func(t *testing.T) {
		actual := Actual{Actions: model.ActionsSettings{
			Enabled:            true,
			AllowedActions:     "all",
			SHAPinningRequired: true,
			PermissionsSource:  model.SettingsSource{Status: model.StatusOK},
			// Workflow permissions and fork approval unreadable.
			WorkflowPermissionsSource: model.SettingsSource{Status: model.StatusForbidden, Message: "needs admin"},
			ForkPRApprovalSource:      model.SettingsSource{Status: model.StatusDisabled, Message: "not enabled or no access"},
		}}
		rep := Evaluate("o", "r", "cfg", cfg, actual)
		if got := rep.Count(model.CompliancePass); got != 3 {
			t.Errorf("the readable permissions group should pass, got %d pass: %+v", got, rep.Checks)
		}
		if got := rep.Count(model.ComplianceSkipped); got != 3 {
			t.Errorf("unreadable groups should be skipped, got %d skipped: %+v", got, rep.Checks)
		}
		if rep.HasFailures() {
			t.Error("unreadable actions settings must not be a failure")
		}
	})
}

func TestEvaluateRepositoryMergeMessagePolicies(t *testing.T) {
	cfg := Config{Repository: &RepositoryConfig{
		SquashMergeCommitTitle:   new("PR_TITLE"),
		SquashMergeCommitMessage: new("PR_BODY"),
		IsTemplate:               new(false),
		AllowForking:             new(true),
	}}
	actual := Actual{Settings: model.RepoSettings{
		SquashMergeCommitTitle:   "COMMIT_OR_PR_TITLE", // drift
		SquashMergeCommitMessage: "PR_BODY",
		IsTemplate:               false,
		AllowForking:             true,
		MergeSettingsSource:      model.SettingsSource{Status: model.StatusOK},
	}}
	rep := Evaluate("o", "r", "cfg", cfg, actual)
	if got := rep.Count(model.ComplianceFail); got != 1 {
		t.Errorf("want 1 fail (squash title), got %d: %+v", got, rep.Checks)
	}
	if got := rep.Count(model.CompliancePass); got != 3 {
		t.Errorf("want 3 passes, got %d: %+v", got, rep.Checks)
	}

	// With the merge group unreadable, the format policies are skipped too but
	// is_template / allow_forking still compare.
	actual.Settings.MergeSettingsSource = model.SettingsSource{Status: model.StatusForbidden, Message: "needs a classic PAT"}
	rep = Evaluate("o", "r", "cfg", cfg, actual)
	if got := rep.Count(model.ComplianceSkipped); got != 2 {
		t.Errorf("format policies should be skipped with the merge group, got %d skipped: %+v", got, rep.Checks)
	}
	if got := rep.Count(model.CompliancePass); got != 2 {
		t.Errorf("is_template/allow_forking should still pass, got %d: %+v", got, rep.Checks)
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
