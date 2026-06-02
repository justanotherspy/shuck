package compliance

import (
	"bytes"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

// fullActual is a readable, fully-populated live-settings fixture.
func fullActual() Actual {
	return Actual{
		Settings: model.RepoSettings{
			Visibility:                   "public",
			DefaultBranch:                "main",
			AllowMergeCommit:             false,
			AllowSquashMerge:             true,
			DeleteBranchOnMerge:          true,
			HasIssues:                    true,
			SecretScanning:               "enabled",
			SecretScanningPushProtection: "enabled",
			DependabotSecurityUpdates:    "disabled",
			SecuritySource:               model.SettingsSource{Status: model.StatusOK},
			MergeSettingsSource:          model.SettingsSource{Status: model.StatusOK},
		},
		VulnAlerts: true,
		VulnSource: model.SettingsSource{Status: model.StatusOK},
		Branches: map[string]Branch{
			"main": {
				Protection: model.BranchProtection{
					Branch: "main", Protected: true,
					RequiredPullRequestReviews:   true,
					RequiredApprovingReviewCount: 2,
					DismissStaleReviews:          true,
					EnforceAdmins:                true,
					RequiredStatusChecks:         []string{"test", "lint"},
					StrictStatusChecks:           true,
				},
				Source: model.SettingsSource{Status: model.StatusOK},
			},
		},
	}
}

func TestDiscoverCreatesFullSnapshot(t *testing.T) {
	disc, err := Discover(nil, fullActual())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !disc.Created || !disc.Changed {
		t.Errorf("a new config should be Created and Changed: %+v", disc)
	}
	if len(disc.Changes) != 0 {
		t.Errorf("create mode records no per-key changes, got %v", disc.Changes)
	}

	// The generated document must round-trip through Parse (no unknown keys, not
	// empty) and reflect the live values.
	cfg, err := Parse(disc.Data)
	if err != nil {
		t.Fatalf("generated config does not parse: %v\n%s", err, disc.Data)
	}
	r := cfg.Repository
	if r == nil || r.Visibility == nil || *r.Visibility != "public" {
		t.Errorf("visibility not snapshotted: %+v", r)
	}
	if r.AllowSquashMerge == nil || !*r.AllowSquashMerge || r.AllowMergeCommit == nil || *r.AllowMergeCommit {
		t.Errorf("merge settings not snapshotted: %+v", r)
	}
	sec := cfg.Security
	if sec == nil || sec.SecretScanning == nil || !*sec.SecretScanning {
		t.Errorf("secret_scanning not snapshotted: %+v", sec)
	}
	if sec.DependabotSecurityUpdates == nil || *sec.DependabotSecurityUpdates {
		t.Errorf("disabled dependabot updates should snapshot as false: %+v", sec)
	}
	if sec.VulnerabilityAlerts == nil || !*sec.VulnerabilityAlerts {
		t.Errorf("vulnerability_alerts not snapshotted: %+v", sec)
	}
	bc := cfg.BranchProtection["main"]
	if bc == nil {
		t.Fatalf("main branch protection not snapshotted:\n%s", disc.Data)
	}
	if bc.RequiredApprovingReviewCount == nil || *bc.RequiredApprovingReviewCount != 2 {
		t.Errorf("review count not snapshotted: %+v", bc)
	}
	if bc.RequiredStatusChecks == nil || strings.Join(*bc.RequiredStatusChecks, ",") != "lint,test" {
		t.Errorf("status checks should be snapshotted sorted: %+v", bc.RequiredStatusChecks)
	}

	// Evaluating the generated config against the same live settings must be
	// fully compliant: that is the whole point of a snapshot.
	rep := Evaluate("o", "r", "generated", cfg, fullActual())
	if !rep.Compliant() {
		for _, c := range rep.Checks {
			if c.Status != model.CompliancePass {
				t.Errorf("generated config check not passing: %+v", c)
			}
		}
	}
}

func TestDiscoverCreateOmitsUnreadable(t *testing.T) {
	actual := fullActual()
	actual.Settings.SecuritySource = model.SettingsSource{Status: model.StatusForbidden, Message: "needs admin"}
	actual.VulnSource = model.SettingsSource{Status: model.StatusForbidden, Message: "needs admin"}
	actual.Branches["main"] = Branch{
		Protection: model.BranchProtection{Branch: "main"},
		Source:     model.SettingsSource{Status: model.StatusForbidden, Message: "needs admin"},
	}

	disc, err := Discover(nil, actual)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	cfg, err := Parse(disc.Data)
	if err != nil {
		t.Fatalf("generated config does not parse: %v\n%s", err, disc.Data)
	}
	if cfg.Security != nil {
		t.Errorf("unreadable security settings should be omitted: %+v", cfg.Security)
	}
	if len(cfg.BranchProtection) != 0 {
		t.Errorf("unreadable branch protection should be omitted: %+v", cfg.BranchProtection)
	}
	if len(disc.Notes) != 3 {
		t.Errorf("want 3 omission notes, got %v", disc.Notes)
	}
}

func TestDiscoverCreateSkipsUnprotectedBranch(t *testing.T) {
	actual := fullActual()
	actual.Branches["main"] = Branch{
		Protection: model.BranchProtection{Branch: "main", Protected: false},
		Source:     model.SettingsSource{Status: model.StatusOK},
	}
	disc, err := Discover(nil, actual)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	cfg, err := Parse(disc.Data)
	if err != nil {
		t.Fatalf("generated config does not parse: %v\n%s", err, disc.Data)
	}
	if len(cfg.BranchProtection) != 0 {
		t.Errorf("unprotected branch should not be declared: %+v", cfg.BranchProtection)
	}
	if len(disc.Notes) != 1 || !strings.Contains(disc.Notes[0], "no protection rule") {
		t.Errorf("want an unprotected-branch note, got %v", disc.Notes)
	}
}

const existingConfig = `# Header comment that must survive an update.
repository:
  visibility: public
  allow_merge_commit: true # inline comment kept
  delete_branch_on_merge: false

security:
  secret_scanning: false

branch_protection:
  main:
    required_approving_review_count: 1
    enforce_admins: false
    required_status_checks:
      - build
`

func TestDiscoverUpdateSyncsDrift(t *testing.T) {
	disc, err := Discover([]byte(existingConfig), fullActual())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if disc.Created {
		t.Error("an existing config must not be reported as created")
	}
	if !disc.Changed {
		t.Error("drift should mark the discovery as changed")
	}

	// Drifted: allow_merge_commit (true→false), delete_branch_on_merge
	// (false→true), secret_scanning (false→true), main review count (1→2),
	// enforce_admins (false→true), status checks ([build]→[lint test]).
	// Unchanged: visibility.
	if len(disc.Changes) != 6 {
		t.Errorf("want 6 changes, got %d: %+v", len(disc.Changes), disc.Changes)
	}

	got := string(disc.Data)
	for _, want := range []string{
		"# Header comment that must survive an update.",
		"# inline comment kept",
		"allow_merge_commit: false",
		"delete_branch_on_merge: true",
		"secret_scanning: true",
		"required_approving_review_count: 2",
		"enforce_admins: true",
		"visibility: public",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("updated config missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "- lint") || !strings.Contains(got, "- test") || strings.Contains(got, "- build") {
		t.Errorf("status checks not synced:\n%s", got)
	}

	// The updated config must itself parse and now be compliant.
	cfg, err := Parse(disc.Data)
	if err != nil {
		t.Fatalf("updated config does not parse: %v\n%s", err, disc.Data)
	}
	rep := Evaluate("o", "r", "updated", cfg, fullActual())
	if !rep.Compliant() {
		for _, c := range rep.Checks {
			if c.Status != model.CompliancePass {
				t.Errorf("updated config check not passing: %+v", c)
			}
		}
	}
}

func TestDiscoverUpdateNoDrift(t *testing.T) {
	// Snapshot, then discover again over the snapshot: nothing should change,
	// and the bytes must be returned untouched.
	first, err := Discover(nil, fullActual())
	if err != nil {
		t.Fatalf("Discover (create): %v", err)
	}
	second, err := Discover(first.Data, fullActual())
	if err != nil {
		t.Fatalf("Discover (update): %v", err)
	}
	if second.Created || second.Changed || len(second.Changes) != 0 {
		t.Errorf("no drift expected: %+v", second.Changes)
	}
	if !bytes.Equal(second.Data, first.Data) {
		t.Errorf("an up-to-date config must be returned byte-identical")
	}
}

func TestDiscoverUpdateKeepsUndeclaredKeysUndeclared(t *testing.T) {
	existing := "repository:\n  allow_merge_commit: true\n"
	disc, err := Discover([]byte(existing), fullActual())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	cfg, err := Parse(disc.Data)
	if err != nil {
		t.Fatalf("updated config does not parse: %v\n%s", err, disc.Data)
	}
	if cfg.Repository.Visibility != nil || cfg.Security != nil || len(cfg.BranchProtection) != 0 {
		t.Errorf("update must not add undeclared keys:\n%s", disc.Data)
	}
	if cfg.Repository.AllowMergeCommit == nil || *cfg.Repository.AllowMergeCommit {
		t.Errorf("declared key not synced:\n%s", disc.Data)
	}
}

func TestDiscoverUpdateLeavesUnreadableAlone(t *testing.T) {
	existing := `security:
  secret_scanning: true
  vulnerability_alerts: true
branch_protection:
  main:
    enforce_admins: true
`
	actual := fullActual()
	actual.Settings.SecretScanning = "disabled" // would drift, but...
	actual.Settings.SecuritySource = model.SettingsSource{Status: model.StatusForbidden, Message: "needs admin"}
	actual.VulnAlerts = false
	actual.VulnSource = model.SettingsSource{Status: model.StatusForbidden, Message: "needs admin"}
	actual.Branches["main"] = Branch{
		Protection: model.BranchProtection{Branch: "main"},
		Source:     model.SettingsSource{Status: model.StatusForbidden, Message: "needs admin"},
	}

	disc, err := Discover([]byte(existing), actual)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if disc.Changed || len(disc.Changes) != 0 {
		t.Errorf("unreadable settings must not be synced: %+v", disc.Changes)
	}
	if string(disc.Data) != existing {
		t.Errorf("config must be untouched:\n%s", disc.Data)
	}
	if len(disc.Notes) != 3 {
		t.Errorf("want 3 left-unchanged notes, got %v", disc.Notes)
	}
}

func TestDiscoverCreateOmitsMergeSettingsWhenUnreadable(t *testing.T) {
	actual := fullActual()
	actual.Settings.MergeSettingsSource = model.SettingsSource{Status: model.StatusForbidden, Message: "needs a classic PAT"}

	disc, err := Discover(nil, actual)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	cfg, err := Parse(disc.Data)
	if err != nil {
		t.Fatalf("generated config does not parse: %v\n%s", err, disc.Data)
	}
	r := cfg.Repository
	if r.AllowSquashMerge != nil || r.AllowMergeCommit != nil || r.DeleteBranchOnMerge != nil {
		t.Errorf("unreadable merge settings must be omitted: %+v", r)
	}
	if r.Visibility == nil || r.HasWiki == nil {
		t.Errorf("readable settings must still be snapshotted: %+v", r)
	}
	found := false
	for _, n := range disc.Notes {
		if strings.Contains(n, "merge settings omitted") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a merge-settings omission note, got %v", disc.Notes)
	}
}

func TestDiscoverUpdateLeavesMergeSettingsAloneWhenUnreadable(t *testing.T) {
	existing := "repository:\n  allow_squash_merge: true\n  delete_branch_on_merge: true\n  has_wiki: true\n"
	actual := fullActual()
	actual.Settings.HasWiki = false // readable drift: synced
	actual.Settings.AllowSquashMerge = false
	actual.Settings.DeleteBranchOnMerge = false
	actual.Settings.MergeSettingsSource = model.SettingsSource{Status: model.StatusForbidden, Message: "needs a classic PAT"}

	disc, err := Discover([]byte(existing), actual)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := string(disc.Data)
	// The invisible all-false merge values must not overwrite the declared intent.
	if !strings.Contains(got, "allow_squash_merge: true") || !strings.Contains(got, "delete_branch_on_merge: true") {
		t.Errorf("unreadable merge settings must not be synced:\n%s", got)
	}
	if !strings.Contains(got, "has_wiki: false") {
		t.Errorf("readable drift should still be synced:\n%s", got)
	}
	if len(disc.Changes) != 1 {
		t.Errorf("want only the has_wiki change, got %+v", disc.Changes)
	}
	found := false
	for _, n := range disc.Notes {
		if strings.Contains(n, "merge settings left unchanged") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a merge-settings note, got %v", disc.Notes)
	}
}

func TestDiscoverRulesetOnlyBranch(t *testing.T) {
	rulesetBranch := Branch{
		Protection: model.BranchProtection{
			Branch: "main", Protected: true, ViaRulesetsOnly: true,
			RequiredPullRequestReviews:   true,
			RequiredApprovingReviewCount: 1,
			RequiredSignatures:           true,
			AllowForcePushes:             false,
			AllowDeletions:               false,
		},
		Source: model.SettingsSource{Status: model.StatusOK},
	}

	t.Run("create omits enforce_admins", func(t *testing.T) {
		actual := fullActual()
		actual.Branches["main"] = rulesetBranch
		cfg, _ := FromActual(actual)
		bc := cfg.BranchProtection["main"]
		if bc == nil {
			t.Fatal("ruleset-protected branch should be declared")
		}
		if bc.EnforceAdmins != nil {
			t.Errorf("enforce_admins is unknowable for ruleset-only protection: %+v", bc)
		}
		if bc.RequiredSignatures == nil || !*bc.RequiredSignatures {
			t.Errorf("ruleset protections should be snapshotted: %+v", bc)
		}
	})

	t.Run("update keeps declared enforce_admins", func(t *testing.T) {
		existing := "branch_protection:\n  main:\n    enforce_admins: true\n    required_approving_review_count: 2\n"
		actual := fullActual()
		actual.Branches["main"] = rulesetBranch

		disc, err := Discover([]byte(existing), actual)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		got := string(disc.Data)
		if !strings.Contains(got, "enforce_admins: true") {
			t.Errorf("declared enforce_admins must not be synced from ruleset-only protection:\n%s", got)
		}
		if !strings.Contains(got, "required_approving_review_count: 1") {
			t.Errorf("ruleset review count should be synced:\n%s", got)
		}
		found := false
		for _, n := range disc.Notes {
			if strings.Contains(n, "enforce_admins left unchanged") {
				found = true
			}
		}
		if !found {
			t.Errorf("want an enforce_admins note, got %v", disc.Notes)
		}
	})
}

func TestDiscoverUpdateUnprotectedBranchKept(t *testing.T) {
	existing := `branch_protection:
  main:
    enforce_admins: true
    allow_force_pushes: false
`
	actual := fullActual()
	actual.Branches["main"] = Branch{
		Protection: model.BranchProtection{Branch: "main", Protected: false},
		Source:     model.SettingsSource{Status: model.StatusOK},
	}

	disc, err := Discover([]byte(existing), actual)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if disc.Changed {
		t.Errorf("losing protection must not delete the declared rule: %+v", disc.Changes)
	}
	if len(disc.Notes) != 1 || !strings.Contains(disc.Notes[0], "no protection rule") {
		t.Errorf("want an unprotected note, got %v", disc.Notes)
	}
}

func TestDiscoverInvalidExistingConfig(t *testing.T) {
	if _, err := Discover([]byte("repository:\n  allow_merg_commit: true\n"), fullActual()); err == nil {
		t.Fatal("a typo'd existing config must be an error, not silently rewritten")
	}
}

func TestFromActualBranchWithoutReviewsOrChecks(t *testing.T) {
	actual := fullActual()
	actual.Branches["main"] = Branch{
		Protection: model.BranchProtection{
			Branch: "main", Protected: true,
			EnforceAdmins: true,
			// No PR reviews required, no status checks.
		},
		Source: model.SettingsSource{Status: model.StatusOK},
	}
	cfg, _ := FromActual(actual)
	bc := cfg.BranchProtection["main"]
	if bc == nil {
		t.Fatal("protected branch should be declared")
	}
	if bc.RequiredApprovingReviewCount != nil || bc.DismissStaleReviews != nil {
		t.Errorf("review settings should be omitted when reviews are not required: %+v", bc)
	}
	if bc.RequiredStatusChecks != nil || bc.StrictStatusChecks != nil {
		t.Errorf("status-check settings should be omitted when none are required: %+v", bc)
	}
	if bc.EnforceAdmins == nil || !*bc.EnforceAdmins {
		t.Errorf("enforce_admins should be snapshotted: %+v", bc)
	}
}

func TestEqualSets(t *testing.T) {
	for _, tt := range []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{[]string{"a", "b"}, []string{"b", "a"}, true},
		{[]string{"a"}, []string{"a", "a"}, false},
		{[]string{"a"}, []string{"b"}, false},
	} {
		if got := equalSets(tt.a, tt.b); got != tt.want {
			t.Errorf("equalSets(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}
