package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/justanotherspy/shuck/internal/compliance"
	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/target"
)

// defaultComplianceConfig is the in-repo path shuck reads the intended settings
// from when --config is not given.
const defaultComplianceConfig = ".github/compliance.yml"

// complianceLister is the slice of gh.Client the compliance command needs. It is
// an interface so tests can stub the network.
type complianceLister interface {
	RepoSettings(ctx context.Context, owner, repo string) (model.RepoSettings, error)
	VulnerabilityAlertsEnabled(ctx context.Context, owner, repo string) (bool, model.SettingsSource)
	BranchProtectionSettings(ctx context.Context, owner, repo, branch string) (model.BranchProtection, model.SettingsSource)
	FileContent(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
}

// newComplianceLister builds the client used by `shuck compliance`. It is a
// package var so tests can supply a stub without hitting GitHub.
var newComplianceLister = func(token string) complianceLister { return gh.New(token) }

// ComplianceOptions tunes a compliance check.
type ComplianceOptions struct {
	ConfigPath  string // explicit path to a compliance config file (overrides discovery)
	Ref         string // git ref to fetch a remote config from (default branch when empty)
	Token       string
	PreferLocal bool // try the local .github/compliance.yml before fetching from the repo
}

const complianceUsage = `shuck compliance — check a repository's settings against its .github/compliance.yml.

Usage:
  shuck compliance                 the repo of the local working directory
  shuck compliance <owner>/<repo>  an explicit repository
  shuck compliance <url>           a github.com/<owner>/<repo>[/...] URL

The .github/compliance.yml file is the definitive statement of a repo's intended
settings (merge options, features, security, branch protection). shuck reads the
repo's live settings via the GitHub API and reports, per setting, whether they
match — so a CI job can gate on drift.

Config discovery (override with --config): for the local repo, the checked-out
.github/compliance.yml is used; for an explicit repo, it is fetched from that
repo (use --ref to pick a branch/tag/SHA). Only the keys the file declares are
checked. A setting the token cannot read (branch protection and security need
admin/repo access) is reported as skipped, never a silent pass.

Exit: 0 when compliant, 1 when a setting drifted, 2 on an operational error.
--exit-zero always exits 0 (report-only).

Auth: set GITHUB_TOKEN (or GH_TOKEN), or pass --token. Reading branch protection
and security settings requires a token with the repo scope and admin access.

Flags:
`

// runCompliance implements `shuck compliance [owner/repo | url]`.
func runCompliance(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck compliance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		jsonOut  bool
		token    string
		config   string
		ref      string
		exitZero bool
	)
	fs.BoolVar(&jsonOut, "json", false, "emit machine-readable JSON instead of the text summary")
	fs.StringVar(&token, "token", "", "GitHub token (overrides GITHUB_TOKEN/GH_TOKEN)")
	fs.StringVar(&config, "config", "", "path to the compliance config (default: discover "+defaultComplianceConfig+")")
	fs.StringVar(&ref, "ref", "", "git ref to fetch a remote config from (default: the repo's default branch)")
	fs.BoolVar(&exitZero, "exit-zero", false, "always exit 0, even when settings drift (report-only)")
	fs.Usage = func() {
		fmt.Fprint(stderr, complianceUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
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

	report, err := Compliance(context.Background(), owner, repo, ComplianceOptions{
		ConfigPath:  config,
		Ref:         ref,
		Token:       token,
		PreferLocal: len(fs.Args()) == 0, // a bare `shuck compliance` checks the local checkout
	})
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}

	if jsonOut {
		if err := compliance.EncodeJSON(stdout, report); err != nil {
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
	} else {
		compliance.Render(stdout, report)
	}

	if !exitZero && report.HasFailures() {
		return 1
	}
	return 0
}

// Compliance loads the repo's intended-settings config, fetches its live
// settings from GitHub, and evaluates the two into a report. It is exported so
// the MCP front-end shares the CLI's pipeline. A missing/invalid config or an
// unreadable repository is a fatal error; an individual setting the token cannot
// see is reported as a skipped check inside the report.
func Compliance(ctx context.Context, owner, repo string, opts ComplianceOptions) (*model.ComplianceReport, error) {
	token := opts.Token
	if token == "" {
		token = tokenFromEnv()
	}
	lister := newComplianceLister(token)

	data, source, err := loadComplianceConfig(ctx, lister, owner, repo, opts)
	if err != nil {
		return nil, err
	}
	cfg, err := compliance.Parse(data)
	if err != nil {
		return nil, err
	}

	settings, err := lister.RepoSettings(ctx, owner, repo)
	if err != nil {
		return nil, err
	}

	actual := compliance.Actual{
		Settings:   settings,
		VulnSource: model.SettingsSource{Status: model.StatusOK},
		Branches:   make(map[string]compliance.Branch, len(cfg.BranchProtection)),
	}
	// The vulnerability-alerts state lives on its own endpoint, so only pay for it
	// when the config actually asserts it.
	if cfg.Security != nil && cfg.Security.VulnerabilityAlerts != nil {
		actual.VulnAlerts, actual.VulnSource = lister.VulnerabilityAlertsEnabled(ctx, owner, repo)
	}
	for name := range cfg.BranchProtection {
		bp, src := lister.BranchProtectionSettings(ctx, owner, repo, name)
		actual.Branches[name] = compliance.Branch{Protection: bp, Source: src}
	}

	return compliance.Evaluate(owner, repo, source, cfg, actual), nil
}

// loadComplianceConfig resolves the config bytes and a human-readable source
// label. With --config it reads that path. Otherwise, for the local repo it
// prefers the checked-out file and falls back to fetching it from the repo; for
// an explicit repo it fetches from the repo directly.
func loadComplianceConfig(ctx context.Context, lister complianceLister, owner, repo string, opts ComplianceOptions) (data []byte, source string, err error) {
	if opts.ConfigPath != "" {
		data, err = os.ReadFile(opts.ConfigPath)
		if err != nil {
			return nil, "", fmt.Errorf("read --config %s: %w", opts.ConfigPath, err)
		}
		return data, opts.ConfigPath, nil
	}

	if opts.PreferLocal {
		data, err = os.ReadFile(defaultComplianceConfig)
		if err == nil {
			return data, defaultComplianceConfig, nil
		}
		if !os.IsNotExist(err) {
			return nil, "", fmt.Errorf("read %s: %w", defaultComplianceConfig, err)
		}
		// Fall through to fetching it from the repo.
	}

	data, err = lister.FileContent(ctx, owner, repo, defaultComplianceConfig, opts.Ref)
	if err != nil {
		if gh.FileNotFound(err) {
			return nil, "", fmt.Errorf("no compliance config found: %s is not in %s/%s%s — add it, or pass --config",
				defaultComplianceConfig, owner, repo, refSuffix(opts.Ref))
		}
		return nil, "", err
	}
	return data, remoteSource(owner, repo, opts.Ref), nil
}

func remoteSource(owner, repo, ref string) string {
	s := fmt.Sprintf("github:%s/%s", owner, repo)
	if ref != "" {
		s += "@" + ref
	}
	return s + ":" + defaultComplianceConfig
}

func refSuffix(ref string) string {
	if ref == "" {
		return ""
	}
	return " @" + ref
}
