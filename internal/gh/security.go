package gh

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/go-github/v88/github"

	"github.com/justanotherspy/shuck/internal/model"
)

// ListCodeScanningAlerts lists a repo's code scanning (e.g. CodeQL) alerts for
// the given normalized state. The returned SecuritySource records whether the
// feature was available; a disabled/forbidden source is not an error.
func (c *Client) ListCodeScanningAlerts(ctx context.Context, owner, repo, state string) ([]model.CodeScanningAlert, model.SecuritySource) {
	apiState, ok := codeScanningState(state)
	if !ok {
		return nil, stateNotApplicable(state)
	}
	opts := &github.AlertListOptions{State: apiState, ListOptions: github.ListOptions{PerPage: 100}}
	var out []model.CodeScanningAlert
	for {
		alerts, resp, err := c.gh.CodeScanning.ListAlertsForRepo(ctx, owner, repo, opts)
		if err != nil {
			if src, soft := classifySecurityErr(err); soft {
				return nil, src
			}
			return nil, model.SecuritySource{Status: model.StatusError, Message: err.Error()}
		}
		for _, a := range alerts {
			out = append(out, mapCodeScanningAlert(a))
		}
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, model.SecuritySource{Status: model.StatusOK}
}

// ListSecretScanningAlerts lists a repo's secret scanning alerts for the given
// normalized state, including each alert's file locations. The raw secret value
// is never read.
func (c *Client) ListSecretScanningAlerts(ctx context.Context, owner, repo, state string) ([]model.SecretScanningAlert, model.SecuritySource) {
	apiState, ok := secretScanningState(state)
	if !ok {
		return nil, stateNotApplicable(state)
	}
	opts := &github.SecretScanningAlertListOptions{State: apiState, ListOptions: github.ListOptions{PerPage: 100}}
	var out []model.SecretScanningAlert
	for {
		alerts, resp, err := c.gh.SecretScanning.ListAlertsForRepo(ctx, owner, repo, opts)
		if err != nil {
			if src, soft := classifySecurityErr(err); soft {
				return nil, src
			}
			return nil, model.SecuritySource{Status: model.StatusError, Message: err.Error()}
		}
		for _, a := range alerts {
			m := mapSecretScanningAlert(a)
			m.Locations = c.secretLocations(ctx, owner, repo, m.Number)
			out = append(out, m)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, model.SecuritySource{Status: model.StatusOK}
}

// ListDependabotAlerts lists a repo's Dependabot (vulnerable-dependency) alerts
// for the given normalized state. npm malware advisories surface here.
func (c *Client) ListDependabotAlerts(ctx context.Context, owner, repo, state string) ([]model.DependabotAlert, model.SecuritySource) {
	apiState, ok := dependabotState(state)
	if !ok {
		return nil, stateNotApplicable(state)
	}
	opts := &github.ListAlertsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	if apiState != "" {
		opts.State = new(apiState)
	}
	var out []model.DependabotAlert
	for {
		alerts, resp, err := c.gh.Dependabot.ListRepoAlerts(ctx, owner, repo, opts)
		if err != nil {
			if src, soft := classifySecurityErr(err); soft {
				return nil, src
			}
			return nil, model.SecuritySource{Status: model.StatusError, Message: err.Error()}
		}
		for _, a := range alerts {
			out = append(out, mapDependabotAlert(a))
		}
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, model.SecuritySource{Status: model.StatusOK}
}

// secretLocations fetches the file locations for one secret alert. It is
// best-effort: any error or non-file location is silently skipped.
func (c *Client) secretLocations(ctx context.Context, owner, repo string, number int) []model.SecretLocation {
	opts := &github.ListOptions{PerPage: 100}
	var out []model.SecretLocation
	for {
		locs, resp, err := c.gh.SecretScanning.ListLocationsForAlert(ctx, owner, repo, int64(number), opts)
		if err != nil {
			return out
		}
		for _, l := range locs {
			d := l.GetDetails()
			if d == nil || d.GetPath() == "" {
				continue
			}
			out = append(out, model.SecretLocation{
				Path:      d.GetPath(),
				StartLine: d.GetStartline(),
				EndLine:   d.GetEndLine(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out
}

func mapCodeScanningAlert(a *github.Alert) model.CodeScanningAlert {
	sev := normalizeSeverity(a.GetRule().GetSecuritySeverityLevel())
	if sev == model.SeverityUnknown {
		sev = normalizeSeverity(a.GetRuleSeverity())
	}
	inst := a.GetMostRecentInstance()
	loc := inst.GetLocation()
	return model.CodeScanningAlert{
		Number:      a.GetNumber(),
		State:       a.GetState(),
		Severity:    sev,
		RuleID:      a.GetRuleID(),
		Description: a.GetRuleDescription(),
		Tool:        a.GetTool().GetName(),
		Path:        loc.GetPath(),
		StartLine:   loc.GetStartLine(),
		EndLine:     loc.GetEndLine(),
		Message:     inst.GetMessage().GetText(),
		HTMLURL:     a.GetHTMLURL(),
	}
}

func mapSecretScanningAlert(a *github.SecretScanningAlert) model.SecretScanningAlert {
	return model.SecretScanningAlert{
		Number:      a.GetNumber(),
		State:       a.GetState(),
		SecretType:  a.GetSecretType(),
		DisplayName: a.GetSecretTypeDisplayName(),
		Resolution:  a.GetResolution(),
		HTMLURL:     a.GetHTMLURL(),
	}
}

func mapDependabotAlert(a *github.DependabotAlert) model.DependabotAlert {
	dep := a.GetDependency()
	pkg := dep.GetPackage()
	adv := a.GetSecurityAdvisory()
	vuln := a.GetSecurityVulnerability()
	return model.DependabotAlert{
		Number:             a.GetNumber(),
		State:              a.GetState(),
		Severity:           normalizeSeverity(adv.GetSeverity()),
		Ecosystem:          pkg.GetEcosystem(),
		Package:            pkg.GetName(),
		ManifestPath:       dep.GetManifestPath(),
		VulnerableVersions: vuln.GetVulnerableVersionRange(),
		FixedVersion:       vuln.GetFirstPatchedVersion().GetIdentifier(),
		GHSAID:             adv.GetGHSAID(),
		CVEID:              adv.GetCVEID(),
		Summary:            adv.GetSummary(),
		HTMLURL:            a.GetHTMLURL(),
	}
}

// normalizeSeverity maps the various GitHub severity vocabularies onto shuck's
// single scale. Code scanning's rule "error" maps to high; advisory "moderate"
// maps to medium.
func normalizeSeverity(s string) model.SecuritySeverity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return model.SeverityCritical
	case "high", "error":
		return model.SeverityHigh
	case "medium", "moderate":
		return model.SeverityMedium
	case "low":
		return model.SeverityLow
	case "warning":
		return model.SeverityWarning
	case "note":
		return model.SeverityNote
	default:
		return model.SeverityUnknown
	}
}

// classifySecurityErr maps a GitHub API error to a soft per-source outcome:
// 404 means the feature is not enabled or invisible to the token, 403 means the
// token lacks access. Both let the command keep going with the other sources.
func classifySecurityErr(err error) (model.SecuritySource, bool) {
	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		switch ghErr.Response.StatusCode {
		case http.StatusNotFound:
			return model.SecuritySource{Status: model.StatusDisabled, Message: "not enabled or no access"}, true
		case http.StatusForbidden:
			return model.SecuritySource{Status: model.StatusForbidden, Message: "token lacks access (needs security_events / repo scope)"}, true
		}
	}
	return model.SecuritySource{}, false
}

func stateNotApplicable(state string) model.SecuritySource {
	return model.SecuritySource{Status: model.StatusDisabled, Message: "state \"" + state + "\" not applicable to this source"}
}

// The state mappers translate shuck's normalized --state value into each
// source's own vocabulary. The second return is false when the source has no
// equivalent for the requested state (reported as a disabled source). An empty
// string omits the filter ("all").

func codeScanningState(state string) (string, bool) {
	switch state {
	case "open":
		return "open", true
	case "all":
		return "", true
	case "dismissed":
		return "dismissed", true
	case "fixed":
		return "fixed", true
	default: // resolved
		return "", false
	}
}

func secretScanningState(state string) (string, bool) {
	switch state {
	case "open":
		return "open", true
	case "all":
		return "", true
	case "resolved":
		return "resolved", true
	default: // dismissed, fixed
		return "", false
	}
}

func dependabotState(state string) (string, bool) {
	switch state {
	case "open":
		return "open", true
	case "all":
		return "", true
	case "dismissed":
		return "dismissed", true
	case "fixed":
		return "fixed", true
	default: // resolved
		return "", false
	}
}
