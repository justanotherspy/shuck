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
		HasIssues:                r.GetHasIssues(),
		HasWiki:                  r.GetHasWiki(),
		HasProjects:              r.GetHasProjects(),
		HasDiscussions:           r.GetHasDiscussions(),
		WebCommitSignoffRequired: r.GetWebCommitSignoffRequired(),
		Archived:                 r.GetArchived(),
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

// BranchProtectionSettings reads a branch's protection rule. A 404 means the
// branch is not protected (or does not exist): that is a readable, compliant-or-
// not state (Protected=false), so it returns an OK source with an empty rule. A
// 403 means the token lacks the admin access protection requires, reported as a
// forbidden source so the checks are skipped rather than failing.
func (c *Client) BranchProtectionSettings(ctx context.Context, owner, repo, branch string) (model.BranchProtection, model.SettingsSource) {
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
	bp.RequireLinearHistory = p.GetRequireLinearHistory().Enabled
	bp.AllowForcePushes = p.GetAllowForcePushes().Enabled
	bp.AllowDeletions = p.GetAllowDeletions().Enabled
	bp.RequireConversationResolution = p.GetRequiredConversationResolution().Enabled
	bp.RequiredSignatures = p.GetRequiredSignatures().GetEnabled()
	return bp
}

// FileContent fetches a single file's decoded contents from a repository at the
// given ref (empty ref = the repository's default branch). It is used to read a
// repo's .shuck/compliance.yaml when no local copy is available.
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
