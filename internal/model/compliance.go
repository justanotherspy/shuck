package model

import "time"

// ComplianceStatus is the outcome of a single compliance check: whether the
// repository's actual setting matched the value the config asserted.
type ComplianceStatus string

// Per-check outcomes.
const (
	CompliancePass    ComplianceStatus = "pass"    // actual matches the asserted value
	ComplianceFail    ComplianceStatus = "fail"    // actual differs from the asserted value (drift)
	ComplianceSkipped ComplianceStatus = "skipped" // the actual value could not be read (no access)
	ComplianceError   ComplianceStatus = "error"   // a genuine error evaluating the check
)

// ComplianceCheck records one asserted setting: what the config wanted, what the
// repository actually has, and whether they agree.
type ComplianceCheck struct {
	Category string           `json:"category"` // repository | security | actions | branch_protection
	Setting  string           `json:"setting"`  // e.g. allow_merge_commit, main.required_approving_review_count
	Expected string           `json:"expected"`
	Actual   string           `json:"actual,omitempty"`
	Status   ComplianceStatus `json:"status"`
	Message  string           `json:"message,omitempty"` // why a check was skipped or errored
}

// ComplianceReport is the assembled compliance posture for one repository: every
// setting the config asserted, paired with the repo's actual value.
type ComplianceReport struct {
	Owner        string            `json:"owner"`
	Repo         string            `json:"repo"`
	ConfigSource string            `json:"config_source"` // where the config came from (a path or a github: ref)
	Checks       []ComplianceCheck `json:"checks"`
	CheckedAt    time.Time         `json:"checked_at"`
}

// Count tallies the checks in the given status.
func (r *ComplianceReport) Count(status ComplianceStatus) int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == status {
			n++
		}
	}
	return n
}

// HasFailures reports whether any check drifted from the asserted value. A
// skipped check (one that could not be read) is not a failure on its own.
func (r *ComplianceReport) HasFailures() bool {
	return r.Count(ComplianceFail) > 0 || r.Count(ComplianceError) > 0
}

// Compliant reports whether the repository fully matches its config: at least
// one check ran and none failed or errored.
func (r *ComplianceReport) Compliant() bool {
	return len(r.Checks) > 0 && !r.HasFailures()
}

// SettingsSource records whether a group of repository settings could be read,
// so a check can distinguish "matched" from "could not read it" (e.g. the token
// lacks the admin access required to see security or branch-protection state).
type SettingsSource struct {
	Status  SourceStatus `json:"status"`
	Message string       `json:"message,omitempty"`
}

// RepoSettings is the normalized subset of a repository's settings shuck checks
// for compliance. The security_and_analysis fields are empty strings when the
// token cannot see them (SecuritySource then reports why).
type RepoSettings struct {
	Visibility    string `json:"visibility"`
	DefaultBranch string `json:"default_branch"`

	AllowMergeCommit    bool `json:"allow_merge_commit"`
	AllowSquashMerge    bool `json:"allow_squash_merge"`
	AllowRebaseMerge    bool `json:"allow_rebase_merge"`
	AllowAutoMerge      bool `json:"allow_auto_merge"`
	AllowUpdateBranch   bool `json:"allow_update_branch"`
	DeleteBranchOnMerge bool `json:"delete_branch_on_merge"`

	// Commit-message format policies for squash and merge commits. They belong
	// to the same classic-token-only group as the allow_* merge settings; empty
	// when not returned.
	SquashMergeCommitTitle   string `json:"squash_merge_commit_title,omitempty"`   // PR_TITLE | COMMIT_OR_PR_TITLE
	SquashMergeCommitMessage string `json:"squash_merge_commit_message,omitempty"` // PR_BODY | COMMIT_MESSAGES | BLANK
	MergeCommitTitle         string `json:"merge_commit_title,omitempty"`          // PR_TITLE | MERGE_MESSAGE
	MergeCommitMessage       string `json:"merge_commit_message,omitempty"`        // PR_BODY | PR_TITLE | BLANK

	// MergeSettingsSource reports whether the merge-policy fields above could be
	// read at all. GitHub returns them only to classic tokens with push access —
	// fine-grained PATs and app installation tokens never receive them — so an
	// absent group must be skipped, not read as all-false.
	MergeSettingsSource SettingsSource `json:"merge_settings_source"`

	HasIssues      bool `json:"has_issues"`
	HasWiki        bool `json:"has_wiki"`
	HasProjects    bool `json:"has_projects"`
	HasDiscussions bool `json:"has_discussions"`

	IsTemplate   bool `json:"is_template"`
	AllowForking bool `json:"allow_forking"`

	WebCommitSignoffRequired bool `json:"web_commit_signoff_required"`
	Archived                 bool `json:"archived"`

	// security_and_analysis status strings ("enabled"/"disabled"), empty when
	// the section was not returned (the token is not an admin).
	SecretScanning               string `json:"secret_scanning,omitempty"`
	SecretScanningPushProtection string `json:"secret_scanning_push_protection,omitempty"`
	DependabotSecurityUpdates    string `json:"dependabot_security_updates,omitempty"`

	SecuritySource SettingsSource `json:"security_source"`
}

// ActionsSettings is the normalized GitHub Actions policy state shuck checks
// for compliance. The values come from three separate admin-only endpoints, so
// each group carries its own source: an unreadable group is skipped, never
// reported as its zero value.
type ActionsSettings struct {
	// GET /repos/{owner}/{repo}/actions/permissions
	Enabled            bool           `json:"enabled"`
	AllowedActions     string         `json:"allowed_actions,omitempty"` // all | local_only | selected
	SHAPinningRequired bool           `json:"sha_pinning_required"`
	PermissionsSource  SettingsSource `json:"permissions_source"`

	// GET /repos/{owner}/{repo}/actions/permissions/workflow
	DefaultWorkflowPermissions   string         `json:"default_workflow_permissions,omitempty"` // read | write
	CanApprovePullRequestReviews bool           `json:"can_approve_pull_request_reviews"`
	WorkflowPermissionsSource    SettingsSource `json:"workflow_permissions_source"`

	// GET /repos/{owner}/{repo}/actions/permissions/fork-pr-contributor-approval
	ForkPRContributorApproval string         `json:"fork_pr_contributor_approval,omitempty"` // first_time_contributors_new_to_github | first_time_contributors | all_external_contributors
	ForkPRApprovalSource      SettingsSource `json:"fork_pr_approval_source"`
}

// BranchProtection is the normalized branch-protection state shuck checks. When
// Protected is false the branch has no protection rule (or does not exist), so
// every asserted protection is reported as not satisfied. The state is the
// union of a classic branch-protection rule and the repository rulesets that
// apply to the branch, whichever is stricter.
type BranchProtection struct {
	Branch    string `json:"branch"`
	Protected bool   `json:"protected"`

	// ViaRulesetsOnly is true when the protection comes solely from repository
	// rulesets (no classic branch-protection rule). Classic-only concepts —
	// enforce_admins, whose ruleset equivalent (bypass actors) is not visible via
	// the rules API — are then skipped rather than reported as false.
	ViaRulesetsOnly bool `json:"via_rulesets_only,omitempty"`

	RequiredPullRequestReviews   bool `json:"required_pull_request_reviews"`
	RequiredApprovingReviewCount int  `json:"required_approving_review_count"`
	DismissStaleReviews          bool `json:"dismiss_stale_reviews"`
	RequireCodeOwnerReviews      bool `json:"require_code_owner_reviews"`
	RequireLastPushApproval      bool `json:"require_last_push_approval"`

	RequiredStatusChecks []string `json:"required_status_checks,omitempty"`
	StrictStatusChecks   bool     `json:"strict_status_checks"`

	EnforceAdmins                 bool `json:"enforce_admins"`
	RequireLinearHistory          bool `json:"required_linear_history"`
	AllowForcePushes              bool `json:"allow_force_pushes"`
	AllowDeletions                bool `json:"allow_deletions"`
	RequireConversationResolution bool `json:"required_conversation_resolution"`
	RequiredSignatures            bool `json:"required_signatures"`
}
