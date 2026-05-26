package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/justanotherspy/shuck/internal/cache"
	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/security"
	"github.com/justanotherspy/shuck/internal/target"
)

// securityCacheTTL bounds how long a repo's cached alert list is reused before
// shuck re-fetches it. Security findings change often, so the window is short;
// --refresh forces an immediate re-fetch.
const securityCacheTTL = time.Hour

// securityLister is the slice of gh.Client the security command needs. It is an
// interface so tests can stub the network.
type securityLister interface {
	ListCodeScanningAlerts(ctx context.Context, owner, repo, state string) ([]model.CodeScanningAlert, model.SecuritySource)
	ListSecretScanningAlerts(ctx context.Context, owner, repo, state string) ([]model.SecretScanningAlert, model.SecuritySource)
	ListDependabotAlerts(ctx context.Context, owner, repo, state string) ([]model.DependabotAlert, model.SecuritySource)
	DefaultBranchSHA(ctx context.Context, owner, repo string) (string, error)
}

// newSecurityLister builds the client used by `shuck security`. It is a package
// var so tests can supply a stub without hitting GitHub.
var newSecurityLister = func(token string) securityLister { return gh.New(token) }

// SecurityOptions tunes a security inspection.
type SecurityOptions struct {
	State   string // open|all|dismissed|fixed|resolved (defaults to open)
	Token   string
	Refresh bool
}

const securityUsage = `shuck security — summarize a repository's GitHub security alerts.

Usage:
  shuck security                 the repo of the local working directory
  shuck security <owner>/<repo>  an explicit repository
  shuck security <url>           a github.com/<owner>/<repo>[/...] URL

It lists code scanning, secret scanning, and Dependabot alerts (npm malware
advisories surface as Dependabot alerts) with severity, file:line, the vulnerable
package -> fix version, and CVE/GHSA IDs. Raw secret values are never shown.

A source that is not enabled, or not visible to the token, is reported and
skipped rather than failing the command. Results are cached under ~/.shuck for
an hour; --refresh re-fetches.

Auth: set GITHUB_TOKEN (or GH_TOKEN), or pass --token. Security data on private
repos needs a token with the security_events (or repo) scope.

Flags:
`

// runSecurity implements `shuck security [owner/repo | url]`.
func runSecurity(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck security", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		jsonOut  bool
		token    string
		state    string
		refresh  bool
		exitCode bool
	)
	fs.BoolVar(&jsonOut, "json", false, "emit machine-readable JSON instead of the text summary")
	fs.StringVar(&token, "token", "", "GitHub token (overrides GITHUB_TOKEN/GH_TOKEN)")
	fs.StringVar(&state, "state", "open", "alert state to show: open|all|dismissed|fixed|resolved")
	fs.BoolVar(&refresh, "refresh", false, "ignore the cache and re-fetch alerts")
	fs.BoolVar(&exitCode, "exit-code", false, "exit 1 when open alerts are found (for CI gating)")
	fs.Usage = func() {
		fmt.Fprint(stderr, securityUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	state = strings.ToLower(strings.TrimSpace(state))
	if !validSecurityState(state) {
		fmt.Fprintf(stderr, "shuck: invalid --state %q (want: open|all|dismissed|fixed|resolved)\n", state)
		return 2
	}

	owner, repo, err := target.ResolveRepo(fs.Args())
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}

	if token == "" {
		token = tokenFromEnv()
	}

	report, err := Security(context.Background(), owner, repo, SecurityOptions{State: state, Token: token, Refresh: refresh})
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}

	if token == "" && !anySourceOK(report) {
		fmt.Fprintln(stderr, "shuck: no GitHub token set (GITHUB_TOKEN/GH_TOKEN); security data and private repos usually require one.")
	}

	if jsonOut {
		if err := security.EncodeJSON(stdout, report); err != nil {
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
	} else {
		security.Render(stdout, report)
	}

	if exitCode && hasOpenAlerts(report) {
		return 1
	}
	return 0
}

// Security fetches a repo's security alerts (from the cache when fresh, else
// GitHub), assembles and sorts them. It is exported so the MCP front-end shares
// the CLI's pipeline. An error is returned only when every source genuinely
// failed; disabled/forbidden sources are reported in the report itself.
func Security(ctx context.Context, owner, repo string, opts SecurityOptions) (*model.SecurityReport, error) {
	state := opts.State
	if state == "" {
		state = "open"
	}
	if !validSecurityState(state) {
		return nil, fmt.Errorf("invalid state %q (want: open|all|dismissed|fixed|resolved)", state)
	}

	if dir, err := cache.SecurityDir(owner, repo); err == nil {
		_ = cache.Purge(securityCacheTTL, dir)
	}

	token := opts.Token
	if token == "" {
		token = tokenFromEnv()
	}
	lister := newSecurityLister(token)

	var currentSHA string
	if !opts.Refresh {
		if rep, cachedSHA, fetchedAt, ok, err := cache.LoadSecurityReport(owner, repo, state); err == nil && ok && time.Since(fetchedAt) < securityCacheTTL {
			// Within the TTL, reuse the cache unless the default branch moved. If
			// the cheap SHA check itself fails (e.g. offline), the fresh cache
			// still stands rather than forcing a full re-fetch that would also fail.
			sha, shaErr := lister.DefaultBranchSHA(ctx, owner, repo)
			if shaErr != nil || sha == cachedSHA {
				return rep, nil
			}
			currentSHA = sha
		}
	}

	report := &model.SecurityReport{Owner: owner, Repo: repo, State: state, CheckedAt: time.Now()}
	report.CodeScanningAlerts, report.CodeScanning = lister.ListCodeScanningAlerts(ctx, owner, repo, state)
	report.SecretScanningAlerts, report.SecretScanning = lister.ListSecretScanningAlerts(ctx, owner, repo, state)
	report.DependabotAlerts, report.Dependabot = lister.ListDependabotAlerts(ctx, owner, repo, state)
	security.Sort(report)

	if allSourcesErrored(report) {
		return nil, fmt.Errorf("could not fetch security alerts for %s/%s: %s", owner, repo, report.Dependabot.Message)
	}

	// Don't cache a result that includes a transient error; --refresh aside, a
	// later run within the TTL should retry the failed source.
	if !anySourceError(report) {
		if currentSHA == "" {
			// Best-effort: a failed SHA lookup stores "", forcing the next run
			// onto the TTL path rather than the cheap SHA short-circuit.
			currentSHA, _ = lister.DefaultBranchSHA(ctx, owner, repo)
		}
		if err := cache.SaveSecurityReport(report, currentSHA); err != nil {
			fmt.Fprintln(os.Stderr, "shuck: warning: could not write security cache:", err)
		}
	}
	return report, nil
}

func validSecurityState(s string) bool {
	switch s {
	case "open", "all", "dismissed", "fixed", "resolved":
		return true
	default:
		return false
	}
}

func anySourceOK(r *model.SecurityReport) bool {
	return r.CodeScanning.Status == model.StatusOK ||
		r.SecretScanning.Status == model.StatusOK ||
		r.Dependabot.Status == model.StatusOK
}

func anySourceError(r *model.SecurityReport) bool {
	return r.CodeScanning.Status == model.StatusError ||
		r.SecretScanning.Status == model.StatusError ||
		r.Dependabot.Status == model.StatusError
}

func allSourcesErrored(r *model.SecurityReport) bool {
	return r.CodeScanning.Status == model.StatusError &&
		r.SecretScanning.Status == model.StatusError &&
		r.Dependabot.Status == model.StatusError
}

// hasOpenAlerts reports whether any collected alert is in the open state, so
// --exit-code gates on actionable findings even under a wider --state filter.
func hasOpenAlerts(r *model.SecurityReport) bool {
	for _, a := range r.CodeScanningAlerts {
		if a.State == "open" {
			return true
		}
	}
	for _, a := range r.DependabotAlerts {
		if a.State == "open" {
			return true
		}
	}
	for _, a := range r.SecretScanningAlerts {
		if a.State == "open" {
			return true
		}
	}
	return false
}
