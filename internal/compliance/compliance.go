// Package compliance compares a repository's actual GitHub settings against the
// intended settings declared in its .github/compliance.yml. It does no network
// I/O: the gh layer fetches the live settings, this package parses the config
// and evaluates the two into a model.ComplianceReport for text or JSON output.
//
// The config is partial by design — only the keys it declares are checked, so a
// repo may assert just the handful of settings it cares about. A declared key
// whose live value cannot be read (e.g. the token is not an admin) is reported
// as skipped, never as a silent pass.
package compliance

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/justanotherspy/shuck/internal/model"
)

// Config is the parsed .github/compliance.yml: the repository's intended
// settings. Every field is a pointer (or a map), so an omitted key means "do not
// check this" rather than "expect the zero value". The yaml tags carry omitempty
// so a Config can also be marshaled (by `shuck compliance discover`) without
// emitting nulls for undeclared keys.
type Config struct {
	Repository       *RepositoryConfig        `yaml:"repository,omitempty"`
	Security         *SecurityConfig          `yaml:"security,omitempty"`
	BranchProtection map[string]*BranchConfig `yaml:"branch_protection,omitempty"`
}

// RepositoryConfig declares intended general repository settings.
type RepositoryConfig struct {
	Visibility               *string `yaml:"visibility,omitempty"`
	DefaultBranch            *string `yaml:"default_branch,omitempty"`
	AllowMergeCommit         *bool   `yaml:"allow_merge_commit,omitempty"`
	AllowSquashMerge         *bool   `yaml:"allow_squash_merge,omitempty"`
	AllowRebaseMerge         *bool   `yaml:"allow_rebase_merge,omitempty"`
	AllowAutoMerge           *bool   `yaml:"allow_auto_merge,omitempty"`
	AllowUpdateBranch        *bool   `yaml:"allow_update_branch,omitempty"`
	DeleteBranchOnMerge      *bool   `yaml:"delete_branch_on_merge,omitempty"`
	HasIssues                *bool   `yaml:"has_issues,omitempty"`
	HasWiki                  *bool   `yaml:"has_wiki,omitempty"`
	HasProjects              *bool   `yaml:"has_projects,omitempty"`
	HasDiscussions           *bool   `yaml:"has_discussions,omitempty"`
	WebCommitSignoffRequired *bool   `yaml:"web_commit_signoff_required,omitempty"`
	Archived                 *bool   `yaml:"archived,omitempty"`
}

// SecurityConfig declares intended security-and-analysis settings. The values
// are booleans; GitHub reports each as the string "enabled"/"disabled".
type SecurityConfig struct {
	SecretScanning               *bool `yaml:"secret_scanning,omitempty"`
	SecretScanningPushProtection *bool `yaml:"secret_scanning_push_protection,omitempty"`
	DependabotSecurityUpdates    *bool `yaml:"dependabot_security_updates,omitempty"`
	VulnerabilityAlerts          *bool `yaml:"vulnerability_alerts,omitempty"`
}

// BranchConfig declares intended protection for one branch (keyed by branch
// name under branch_protection).
type BranchConfig struct {
	RequiredApprovingReviewCount  *int      `yaml:"required_approving_review_count,omitempty"`
	DismissStaleReviews           *bool     `yaml:"dismiss_stale_reviews,omitempty"`
	RequireCodeOwnerReviews       *bool     `yaml:"require_code_owner_reviews,omitempty"`
	RequireLastPushApproval       *bool     `yaml:"require_last_push_approval,omitempty"`
	RequiredStatusChecks          *[]string `yaml:"required_status_checks,omitempty"`
	StrictStatusChecks            *bool     `yaml:"strict_status_checks,omitempty"`
	EnforceAdmins                 *bool     `yaml:"enforce_admins,omitempty"`
	RequireLinearHistory          *bool     `yaml:"required_linear_history,omitempty"`
	AllowForcePushes              *bool     `yaml:"allow_force_pushes,omitempty"`
	AllowDeletions                *bool     `yaml:"allow_deletions,omitempty"`
	RequireConversationResolution *bool     `yaml:"required_conversation_resolution,omitempty"`
	RequiredSignatures            *bool     `yaml:"required_signatures,omitempty"`
}

// Parse decodes a .github/compliance.yml document. Unknown keys are rejected so
// a typo (which would otherwise silently skip a check) surfaces as an error.
func Parse(data []byte) (Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse compliance config: %w", err)
	}
	if cfg.Repository == nil && cfg.Security == nil && len(cfg.BranchProtection) == 0 {
		return Config{}, fmt.Errorf("compliance config is empty: declare at least one of repository, security, or branch_protection")
	}
	if vis := repoVisibility(cfg); vis != nil && !validVisibility(*vis) {
		return Config{}, fmt.Errorf("invalid repository.visibility %q (want: public|private|internal)", *vis)
	}
	return cfg, nil
}

// Actual bundles the live settings the gh layer fetched, for one repository.
type Actual struct {
	Settings   model.RepoSettings
	VulnAlerts bool
	VulnSource model.SettingsSource
	Branches   map[string]Branch
}

// Branch is one branch's live protection state plus whether it could be read.
type Branch struct {
	Protection model.BranchProtection
	Source     model.SettingsSource
}

// Evaluate compares the intended config against the actual settings, producing
// one check per declared key (in a stable order: repository, security, then each
// branch). It is pure: callers fetch Actual via the gh layer and render the
// returned report.
func Evaluate(owner, repo, configSource string, cfg Config, actual Actual) *model.ComplianceReport {
	rep := &model.ComplianceReport{Owner: owner, Repo: repo, ConfigSource: configSource}
	e := &evaluator{report: rep}

	if r := cfg.Repository; r != nil {
		s := actual.Settings
		e.str("repository", "visibility", r.Visibility, s.Visibility)
		e.str("repository", "default_branch", r.DefaultBranch, s.DefaultBranch)
		e.bool("repository", "allow_merge_commit", r.AllowMergeCommit, s.AllowMergeCommit)
		e.bool("repository", "allow_squash_merge", r.AllowSquashMerge, s.AllowSquashMerge)
		e.bool("repository", "allow_rebase_merge", r.AllowRebaseMerge, s.AllowRebaseMerge)
		e.bool("repository", "allow_auto_merge", r.AllowAutoMerge, s.AllowAutoMerge)
		e.bool("repository", "allow_update_branch", r.AllowUpdateBranch, s.AllowUpdateBranch)
		e.bool("repository", "delete_branch_on_merge", r.DeleteBranchOnMerge, s.DeleteBranchOnMerge)
		e.bool("repository", "has_issues", r.HasIssues, s.HasIssues)
		e.bool("repository", "has_wiki", r.HasWiki, s.HasWiki)
		e.bool("repository", "has_projects", r.HasProjects, s.HasProjects)
		e.bool("repository", "has_discussions", r.HasDiscussions, s.HasDiscussions)
		e.bool("repository", "web_commit_signoff_required", r.WebCommitSignoffRequired, s.WebCommitSignoffRequired)
		e.bool("repository", "archived", r.Archived, s.Archived)
	}

	if sec := cfg.Security; sec != nil {
		s := actual.Settings
		e.enabled("security", "secret_scanning", sec.SecretScanning, s.SecretScanning, s.SecuritySource)
		e.enabled("security", "secret_scanning_push_protection", sec.SecretScanningPushProtection, s.SecretScanningPushProtection, s.SecuritySource)
		e.enabled("security", "dependabot_security_updates", sec.DependabotSecurityUpdates, s.DependabotSecurityUpdates, s.SecuritySource)
		e.boolSrc("security", "vulnerability_alerts", sec.VulnerabilityAlerts, actual.VulnAlerts, actual.VulnSource)
	}

	for _, name := range sortedBranches(cfg.BranchProtection) {
		bc := cfg.BranchProtection[name]
		if bc == nil {
			continue
		}
		br := actual.Branches[name]
		e.branch(name, bc, br)
	}

	return rep
}

// evaluator accumulates checks against a report, applying the source guard so an
// unreadable setting becomes a skipped check rather than a false pass/fail.
type evaluator struct {
	report *model.ComplianceReport
}

func (e *evaluator) add(c model.ComplianceCheck) { e.report.Checks = append(e.report.Checks, c) }

// bool checks an always-readable boolean repository setting.
func (e *evaluator) bool(category, setting string, want *bool, got bool) {
	if want == nil {
		return
	}
	e.add(compare(category, setting, fmtBool(*want), fmtBool(got)))
}

// str checks an always-readable string repository setting.
func (e *evaluator) str(category, setting string, want *string, got string) {
	if want == nil {
		return
	}
	e.add(compare(category, setting, *want, got))
}

// boolSrc checks a boolean whose live value may be unreadable (src not OK ⇒
// skipped).
func (e *evaluator) boolSrc(category, setting string, want *bool, got bool, src model.SettingsSource) {
	if want == nil {
		return
	}
	if c, skipped := skip(category, setting, fmtBool(*want), src); skipped {
		e.add(c)
		return
	}
	e.add(compare(category, setting, fmtBool(*want), fmtBool(got)))
}

// enabled checks a security_and_analysis setting, whose live value is the string
// "enabled"/"disabled" and is admin-only to read.
func (e *evaluator) enabled(category, setting string, want *bool, got string, src model.SettingsSource) {
	if want == nil {
		return
	}
	if c, skipped := skip(category, setting, fmtEnabled(*want), src); skipped {
		e.add(c)
		return
	}
	actual := got
	if actual == "" {
		actual = "disabled"
	}
	e.add(compare(category, setting, fmtEnabled(*want), actual))
}

// branch evaluates one branch's declared protections against its live state.
func (e *evaluator) branch(name string, bc *BranchConfig, br Branch) {
	category := "branch_protection"
	src := br.Source
	bp := br.Protection

	// A non-OK source (forbidden/error reading protection) skips every declared
	// key for this branch so we never report a false pass.
	if src.Status != model.StatusOK {
		for _, k := range branchSettings(bc) {
			e.add(skipped(category, name+"."+k.setting, k.expected, src.Message))
		}
		return
	}

	// The branch is readable. If it is unprotected, every declared protection is
	// unsatisfied — report each as a fail with a clear actual value.
	if !bp.Protected {
		for _, k := range branchSettings(bc) {
			e.add(model.ComplianceCheck{
				Category: category, Setting: name + "." + k.setting,
				Expected: k.expected, Actual: "branch not protected",
				Status: failIfMeaningful(k), Message: "branch has no protection rule",
			})
		}
		return
	}

	if bc.RequiredApprovingReviewCount != nil {
		e.add(compare(category, name+".required_approving_review_count",
			strconv.Itoa(*bc.RequiredApprovingReviewCount), strconv.Itoa(bp.RequiredApprovingReviewCount)))
	}
	e.bool(category, name+".dismiss_stale_reviews", bc.DismissStaleReviews, bp.DismissStaleReviews)
	e.bool(category, name+".require_code_owner_reviews", bc.RequireCodeOwnerReviews, bp.RequireCodeOwnerReviews)
	e.bool(category, name+".require_last_push_approval", bc.RequireLastPushApproval, bp.RequireLastPushApproval)
	e.bool(category, name+".strict_status_checks", bc.StrictStatusChecks, bp.StrictStatusChecks)
	e.bool(category, name+".enforce_admins", bc.EnforceAdmins, bp.EnforceAdmins)
	e.bool(category, name+".required_linear_history", bc.RequireLinearHistory, bp.RequireLinearHistory)
	e.bool(category, name+".allow_force_pushes", bc.AllowForcePushes, bp.AllowForcePushes)
	e.bool(category, name+".allow_deletions", bc.AllowDeletions, bp.AllowDeletions)
	e.bool(category, name+".required_conversation_resolution", bc.RequireConversationResolution, bp.RequireConversationResolution)
	e.bool(category, name+".required_signatures", bc.RequiredSignatures, bp.RequiredSignatures)
	if bc.RequiredStatusChecks != nil {
		e.add(compareList(category, name+".required_status_checks", *bc.RequiredStatusChecks, bp.RequiredStatusChecks))
	}
}

// branchKey is one declared branch setting, flattened for the unprotected/
// unreadable paths where we emit a uniform check per key.
type branchKey struct {
	setting  string
	expected string
}

// branchSettings flattens a BranchConfig into the declared keys, preserving the
// same field order branch() uses for its detailed comparison.
func branchSettings(bc *BranchConfig) []branchKey {
	var out []branchKey
	if bc.RequiredApprovingReviewCount != nil {
		out = append(out, branchKey{"required_approving_review_count", strconv.Itoa(*bc.RequiredApprovingReviewCount)})
	}
	add := func(name string, b *bool) {
		if b != nil {
			out = append(out, branchKey{name, fmtBool(*b)})
		}
	}
	add("dismiss_stale_reviews", bc.DismissStaleReviews)
	add("require_code_owner_reviews", bc.RequireCodeOwnerReviews)
	add("require_last_push_approval", bc.RequireLastPushApproval)
	add("strict_status_checks", bc.StrictStatusChecks)
	add("enforce_admins", bc.EnforceAdmins)
	add("required_linear_history", bc.RequireLinearHistory)
	add("allow_force_pushes", bc.AllowForcePushes)
	add("allow_deletions", bc.AllowDeletions)
	add("required_conversation_resolution", bc.RequireConversationResolution)
	add("required_signatures", bc.RequiredSignatures)
	if bc.RequiredStatusChecks != nil {
		out = append(out, branchKey{"required_status_checks", strings.Join(*bc.RequiredStatusChecks, ", ")})
	}
	return out
}

// failIfMeaningful decides how an unprotected branch scores a declared key: a
// setting that asks to *relax* protection (allow_force_pushes/allow_deletions =
// true) is technically satisfied when there is no rule, but every other declared
// protection is a failure. We keep it simple and conservative: an unprotected
// branch fails any declared protection, since the intent of declaring it is to
// require the rule to exist.
func failIfMeaningful(_ branchKey) model.ComplianceStatus { return model.ComplianceFail }

// compare builds a pass/fail check from an expected and actual string.
func compare(category, setting, expected, actual string) model.ComplianceCheck {
	status := model.CompliancePass
	if expected != actual {
		status = model.ComplianceFail
	}
	return model.ComplianceCheck{Category: category, Setting: setting, Expected: expected, Actual: actual, Status: status}
}

// compareList compares an unordered set of required status checks.
func compareList(category, setting string, want, got []string) model.ComplianceCheck {
	wantSorted := append([]string(nil), want...)
	gotSorted := append([]string(nil), got...)
	sort.Strings(wantSorted)
	sort.Strings(gotSorted)
	expected := strings.Join(wantSorted, ", ")
	actual := strings.Join(gotSorted, ", ")
	status := model.CompliancePass
	if expected != actual {
		status = model.ComplianceFail
	}
	if actual == "" {
		actual = "(none)"
	}
	return model.ComplianceCheck{Category: category, Setting: setting, Expected: expected, Actual: actual, Status: status}
}

// skip returns a skipped check when src is not OK, so a setting the token cannot
// read is reported honestly instead of passing or failing.
func skip(category, setting, expected string, src model.SettingsSource) (model.ComplianceCheck, bool) {
	if src.Status == model.StatusOK {
		return model.ComplianceCheck{}, false
	}
	return skipped(category, setting, expected, src.Message), true
}

func skipped(category, setting, expected, reason string) model.ComplianceCheck {
	if reason == "" {
		reason = "could not read this setting"
	}
	return model.ComplianceCheck{
		Category: category, Setting: setting, Expected: expected,
		Status: model.ComplianceSkipped, Message: reason,
	}
}

func sortedBranches(m map[string]*BranchConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func repoVisibility(cfg Config) *string {
	if cfg.Repository == nil {
		return nil
	}
	return cfg.Repository.Visibility
}

func validVisibility(v string) bool {
	switch v {
	case "public", "private", "internal":
		return true
	default:
		return false
	}
}

func fmtBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func fmtEnabled(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}
