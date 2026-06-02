package gh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-github/v88/github"

	"github.com/justanotherspy/shuck/internal/model"
)

// RepoSettings reads the repository's general and security settings. The core
// repository fields always come back (a failed Get is a fatal error, since
// nothing else can be checked); the security_and_analysis section is admin-only,
// so when it is absent the returned SecuritySource explains why rather than
// failing the whole check.
func (c *Client) RepoSettings(ctx context.Context, owner, repo string) (model.RepoSettings, error) {
	r, _, err := c.gh.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return model.RepoSettings{}, fmt.Errorf("get repository %s/%s: %w", owner, repo, err)
	}
	s := model.RepoSettings{
		Visibility:               r.GetVisibility(),
		DefaultBranch:            r.GetDefaultBranch(),
		AllowMergeCommit:         r.GetAllowMergeCommit(),
		AllowSquashMerge:         r.GetAllowSquashMerge(),
		AllowRebaseMerge:         r.GetAllowRebaseMerge(),
		AllowAutoMerge:           r.GetAllowAutoMerge(),
		AllowUpdateBranch:        r.GetAllowUpdateBranch(),
		DeleteBranchOnMerge:      r.GetDeleteBranchOnMerge(),
		SquashMergeCommitTitle:   r.GetSquashMergeCommitTitle(),
		SquashMergeCommitMessage: r.GetSquashMergeCommitMessage(),
		MergeCommitTitle:         r.GetMergeCommitTitle(),
		MergeCommitMessage:       r.GetMergeCommitMessage(),
		HasIssues:                r.GetHasIssues(),
		HasWiki:                  r.GetHasWiki(),
		HasProjects:              r.GetHasProjects(),
		HasDiscussions:           r.GetHasDiscussions(),
		IsTemplate:               r.GetIsTemplate(),
		AllowForking:             r.GetAllowForking(),
		WebCommitSignoffRequired: r.GetWebCommitSignoffRequired(),
		Archived:                 r.GetArchived(),
		MergeSettingsSource:      mergeSettingsSource(r),
	}
	if saa := r.GetSecurityAndAnalysis(); saa != nil {
		s.SecretScanning = saa.GetSecretScanning().GetStatus()
		s.SecretScanningPushProtection = saa.GetSecretScanningPushProtection().GetStatus()
		s.DependabotSecurityUpdates = saa.GetDependabotSecurityUpdates().GetStatus()
		s.SecuritySource = model.SettingsSource{Status: model.StatusOK}
	} else {
		// GitHub omits security_and_analysis unless the token has admin access to
		// the repo, so treat its absence as "not visible" rather than "disabled".
		s.SecuritySource = model.SettingsSource{
			Status:  model.StatusForbidden,
			Message: "security settings need admin access (repo or security_events scope)",
		}
	}
	return s, nil
}

// mergeSettingsSource reports whether the repository's merge-policy fields
// (allow_squash_merge & co.) were present in the API response. GitHub includes
// them only for classic tokens with push access; fine-grained PATs and app
// installation tokens never receive them, so go-github's Get* accessors would
// silently read every one as false.
func mergeSettingsSource(r *github.Repository) model.SettingsSource {
	if r.AllowMergeCommit == nil && r.AllowSquashMerge == nil && r.AllowRebaseMerge == nil &&
		r.DeleteBranchOnMerge == nil {
		return model.SettingsSource{
			Status:  model.StatusForbidden,
			Message: "merge settings are not returned for fine-grained or app tokens (use a classic PAT with the repo scope)",
		}
	}
	return model.SettingsSource{Status: model.StatusOK}
}

// VulnerabilityAlertsEnabled reports whether Dependabot vulnerability alerts are
// enabled. It is a separate endpoint from the repository object; a 403/404 is
// reported as a soft source so an unreadable value is skipped, not fatal.
func (c *Client) VulnerabilityAlertsEnabled(ctx context.Context, owner, repo string) (bool, model.SettingsSource) {
	on, _, err := c.gh.Repositories.GetVulnerabilityAlerts(ctx, owner, repo)
	if err != nil {
		if src, soft := classifySettingsErr(err); soft {
			return false, src
		}
		return false, model.SettingsSource{Status: model.StatusError, Message: err.Error()}
	}
	return on, model.SettingsSource{Status: model.StatusOK}
}

// ActionsSettings reads the repository's GitHub Actions policies: whether
// Actions is enabled and which actions may run, the default workflow token
// permissions, and the fork-PR contributor approval policy. The three endpoints
// are admin-only and degrade independently (403/404 ⇒ a non-OK source), so a
// token that cannot read a group skips its checks instead of failing the run.
func (c *Client) ActionsSettings(ctx context.Context, owner, repo string) model.ActionsSettings {
	var s model.ActionsSettings

	perms, _, err := c.gh.Repositories.GetActionsPermissions(ctx, owner, repo)
	if err != nil {
		s.PermissionsSource = settingsSourceFromErr(err)
	} else {
		s.Enabled = perms.GetEnabled()
		s.AllowedActions = perms.GetAllowedActions()
		s.SHAPinningRequired = perms.GetSHAPinningRequired()
		s.PermissionsSource = model.SettingsSource{Status: model.StatusOK}
	}

	wf, _, err := c.gh.Repositories.GetDefaultWorkflowPermissions(ctx, owner, repo)
	if err != nil {
		s.WorkflowPermissionsSource = settingsSourceFromErr(err)
	} else {
		s.DefaultWorkflowPermissions = wf.GetDefaultWorkflowPermissions()
		s.CanApprovePullRequestReviews = wf.GetCanApprovePullRequestReviews()
		s.WorkflowPermissionsSource = model.SettingsSource{Status: model.StatusOK}
	}

	fork, _, err := c.gh.Actions.GetForkPRContributorApprovalPermissions(ctx, owner, repo)
	if err != nil {
		s.ForkPRApprovalSource = settingsSourceFromErr(err)
	} else {
		s.ForkPRContributorApproval = fork.ApprovalPolicy
		s.ForkPRApprovalSource = model.SettingsSource{Status: model.StatusOK}
	}

	return s
}

// settingsSourceFromErr maps an error reading an optional settings endpoint to a
// soft source: 403/404 keep their specific classification, anything else becomes
// an error source. All of them skip the dependent checks rather than fail.
func settingsSourceFromErr(err error) model.SettingsSource {
	if src, soft := classifySettingsErr(err); soft {
		return src
	}
	return model.SettingsSource{Status: model.StatusError, Message: err.Error()}
}

// BranchProtectionSettings reads a branch's effective protection: the classic
// branch-protection rule merged with the repository rulesets that apply to the
// branch. Per setting the stricter source wins, so a protection enforced by
// either mechanism is reported as enforced. A branch governed only by rulesets
// is marked ViaRulesetsOnly so classic-only concepts (enforce_admins) can be
// skipped instead of read as false.
func (c *Client) BranchProtectionSettings(ctx context.Context, owner, repo, branch string) (model.BranchProtection, model.SettingsSource) {
	classic, classicSrc := c.classicBranchProtection(ctx, owner, repo, branch)
	ruleset, rulesOK := c.branchRulesProtection(ctx, owner, repo, branch)

	// No readable ruleset data (or no rules apply): the classic result stands.
	if !rulesOK || !ruleset.Protected {
		return classic, classicSrc
	}
	// Classic protection readable (including the readable "not protected" 404):
	// merge the two; mergeBranchProtection returns the ruleset side when there is
	// no classic rule.
	if classicSrc.Status == model.StatusOK {
		return mergeBranchProtection(classic, ruleset), classicSrc
	}
	// Classic protection unreadable but rulesets are not: report the ruleset
	// protection rather than skipping every check.
	return ruleset, model.SettingsSource{Status: model.StatusOK}
}

// classicBranchProtection reads a branch's classic protection rule. A 404 means
// the branch has no classic rule (or does not exist): that is a readable,
// compliant-or-not state (Protected=false), so it returns an OK source with an
// empty rule. A 403 means the token lacks the admin access protection requires,
// reported as a forbidden source so the checks are skipped rather than failing.
func (c *Client) classicBranchProtection(ctx context.Context, owner, repo, branch string) (model.BranchProtection, model.SettingsSource) {
	p, _, err := c.gh.Repositories.GetBranchProtection(ctx, owner, repo, branch)
	if err != nil {
		var ghErr *github.ErrorResponse
		if errors.As(err, &ghErr) && ghErr.Response != nil {
			switch ghErr.Response.StatusCode {
			case http.StatusNotFound:
				return model.BranchProtection{Branch: branch, Protected: false}, model.SettingsSource{Status: model.StatusOK}
			case http.StatusForbidden:
				return model.BranchProtection{Branch: branch}, model.SettingsSource{
					Status:  model.StatusForbidden,
					Message: "branch protection needs admin access (repo scope)",
				}
			}
		}
		return model.BranchProtection{Branch: branch}, model.SettingsSource{Status: model.StatusError, Message: err.Error()}
	}
	return mapBranchProtection(branch, p), model.SettingsSource{Status: model.StatusOK}
}

// branchRulesProtection reads the repository rules (rulesets) that apply to a
// branch and maps them to the protection model. The second return is false when
// the rules could not be read at all, so the caller can fall back to the classic
// result alone.
func (c *Client) branchRulesProtection(ctx context.Context, owner, repo, branch string) (model.BranchProtection, bool) {
	rules, _, err := c.gh.Repositories.ListRulesForBranch(ctx, owner, repo, branch, &github.ListOptions{PerPage: 100})
	if err != nil {
		return model.BranchProtection{Branch: branch}, false
	}
	return mapBranchRules(branch, rules), true
}

// mapBranchRules maps the effective ruleset rules for a branch onto the
// protection model. Any branch-target rule marks the branch as protected (even
// rule types shuck does not model); force-push / deletion protection is the
// presence of the blocking rule, so its absence means the action is allowed.
func mapBranchRules(branch string, rules *github.BranchRules) model.BranchProtection {
	bp := model.BranchProtection{Branch: branch}
	if rules == nil {
		return bp
	}
	governed := len(rules.PullRequest) > 0 || len(rules.RequiredStatusChecks) > 0 ||
		len(rules.RequiredLinearHistory) > 0 || len(rules.RequiredSignatures) > 0 ||
		len(rules.NonFastForward) > 0 || len(rules.Deletion) > 0 ||
		len(rules.Creation) > 0 || len(rules.Update) > 0 || len(rules.MergeQueue) > 0 ||
		len(rules.Workflows) > 0 || len(rules.CodeScanning) > 0
	if !governed {
		return bp
	}
	bp.Protected = true
	bp.ViaRulesetsOnly = true

	for _, r := range rules.PullRequest {
		if r == nil {
			continue
		}
		p := r.Parameters
		bp.RequiredPullRequestReviews = true
		bp.RequiredApprovingReviewCount = max(bp.RequiredApprovingReviewCount, p.RequiredApprovingReviewCount)
		bp.DismissStaleReviews = bp.DismissStaleReviews || p.DismissStaleReviewsOnPush
		bp.RequireCodeOwnerReviews = bp.RequireCodeOwnerReviews || p.RequireCodeOwnerReview
		bp.RequireLastPushApproval = bp.RequireLastPushApproval || p.RequireLastPushApproval
		bp.RequireConversationResolution = bp.RequireConversationResolution || p.RequiredReviewThreadResolution
	}
	for _, r := range rules.RequiredStatusChecks {
		if r == nil {
			continue
		}
		bp.StrictStatusChecks = bp.StrictStatusChecks || r.Parameters.StrictRequiredStatusChecksPolicy
		for _, chk := range r.Parameters.RequiredStatusChecks {
			if chk != nil {
				bp.RequiredStatusChecks = append(bp.RequiredStatusChecks, chk.Context)
			}
		}
	}
	bp.RequireLinearHistory = len(rules.RequiredLinearHistory) > 0
	bp.RequiredSignatures = len(rules.RequiredSignatures) > 0
	bp.AllowForcePushes = len(rules.NonFastForward) == 0
	bp.AllowDeletions = len(rules.Deletion) == 0
	return bp
}

// mergeBranchProtection combines a classic protection rule with the ruleset
// protection for the same branch: per setting the stricter source wins. When
// only one side protects the branch, that side is returned unchanged.
func mergeBranchProtection(classic, ruleset model.BranchProtection) model.BranchProtection {
	if !ruleset.Protected {
		return classic
	}
	if !classic.Protected {
		return ruleset
	}
	out := classic
	out.ViaRulesetsOnly = false
	out.RequiredPullRequestReviews = classic.RequiredPullRequestReviews || ruleset.RequiredPullRequestReviews
	out.RequiredApprovingReviewCount = max(classic.RequiredApprovingReviewCount, ruleset.RequiredApprovingReviewCount)
	out.DismissStaleReviews = classic.DismissStaleReviews || ruleset.DismissStaleReviews
	out.RequireCodeOwnerReviews = classic.RequireCodeOwnerReviews || ruleset.RequireCodeOwnerReviews
	out.RequireLastPushApproval = classic.RequireLastPushApproval || ruleset.RequireLastPushApproval
	out.RequiredStatusChecks = unionStrings(classic.RequiredStatusChecks, ruleset.RequiredStatusChecks)
	out.StrictStatusChecks = classic.StrictStatusChecks || ruleset.StrictStatusChecks
	out.RequireLinearHistory = classic.RequireLinearHistory || ruleset.RequireLinearHistory
	// The "allow" flags grant a permission, so both sources must allow it.
	out.AllowForcePushes = classic.AllowForcePushes && ruleset.AllowForcePushes
	out.AllowDeletions = classic.AllowDeletions && ruleset.AllowDeletions
	out.RequireConversationResolution = classic.RequireConversationResolution || ruleset.RequireConversationResolution
	out.RequiredSignatures = classic.RequiredSignatures || ruleset.RequiredSignatures
	return out
}

// unionStrings merges two string slices, keeping first-seen order and dropping
// duplicates.
func unionStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func mapBranchProtection(branch string, p *github.Protection) model.BranchProtection {
	bp := model.BranchProtection{Branch: branch, Protected: true}
	if rev := p.GetRequiredPullRequestReviews(); rev != nil {
		bp.RequiredPullRequestReviews = true
		bp.RequiredApprovingReviewCount = rev.RequiredApprovingReviewCount
		bp.DismissStaleReviews = rev.DismissStaleReviews
		bp.RequireCodeOwnerReviews = rev.RequireCodeOwnerReviews
		bp.RequireLastPushApproval = rev.RequireLastPushApproval
	}
	if rsc := p.GetRequiredStatusChecks(); rsc != nil {
		bp.StrictStatusChecks = rsc.Strict
		if rsc.Checks != nil {
			for _, chk := range *rsc.Checks {
				if chk != nil {
					bp.RequiredStatusChecks = append(bp.RequiredStatusChecks, chk.Context)
				}
			}
		} else if rsc.Contexts != nil {
			bp.RequiredStatusChecks = append(bp.RequiredStatusChecks, *rsc.Contexts...)
		}
	}
	bp.EnforceAdmins = p.GetEnforceAdmins().GetEnabled()
	bp.RequireLinearHistory = p.GetRequireLinearHistory().GetEnabled()
	bp.AllowForcePushes = p.GetAllowForcePushes().GetEnabled()
	bp.AllowDeletions = p.GetAllowDeletions().GetEnabled()
	bp.RequireConversationResolution = p.GetRequiredConversationResolution().GetEnabled()
	bp.RequiredSignatures = p.GetRequiredSignatures().GetEnabled()
	return bp
}

// FileContent fetches a single file's decoded contents from a repository at the
// given ref (empty ref = the repository's default branch). It is used to read a
// repo's .github/compliance.yml when no local copy is available.
func (c *Client) FileContent(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	var opts *github.RepositoryContentGetOptions
	if ref != "" {
		opts = &github.RepositoryContentGetOptions{Ref: ref}
	}
	fc, _, _, err := c.gh.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		return nil, fmt.Errorf("get %s from %s/%s: %w", path, owner, repo, err)
	}
	if fc == nil {
		return nil, fmt.Errorf("%s in %s/%s is not a file", path, owner, repo)
	}
	content, err := fc.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decode %s from %s/%s: %w", path, owner, repo, err)
	}
	return []byte(content), nil
}

// classifySettingsErr maps a GitHub API error to a soft per-source outcome the
// same way the security layer does: 404 ⇒ not enabled / not visible, 403 ⇒ the
// token lacks access. Both let compliance skip the setting instead of failing.
func classifySettingsErr(err error) (model.SettingsSource, bool) {
	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		switch ghErr.Response.StatusCode {
		case http.StatusNotFound:
			return model.SettingsSource{Status: model.StatusDisabled, Message: "not enabled or no access"}, true
		case http.StatusForbidden:
			return model.SettingsSource{Status: model.StatusForbidden, Message: "token lacks access (needs repo scope)"}, true
		}
	}
	return model.SettingsSource{}, false
}

// FileNotFound reports whether err is a GitHub 404, so callers fetching an
// optional file (the remote compliance config) can give a clear "not found"
// message instead of a raw API error.
func FileNotFound(err error) bool {
	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		return ghErr.Response.StatusCode == http.StatusNotFound
	}
	// GetContents wraps the error; fall back to a substring check.
	return strings.Contains(err.Error(), "404")
}
