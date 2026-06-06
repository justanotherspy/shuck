package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"

	"github.com/justanotherspy/shuck/internal/dependabot"
	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/target"
)

// Dependabot config paths. GitHub only reads the .yml spelling; the .yaml
// variant is recognized so the audit can flag it as ignored.
const (
	defaultDependabotConfig = ".github/dependabot.yml"
	altDependabotConfig     = ".github/dependabot.yaml"
)

// dependabotLister is the slice of gh.Client the dependabot command needs: it
// reads the config file and lists the repo's file tree. An interface so tests
// can stub the network.
type dependabotLister interface {
	FileContent(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
	RepoTree(ctx context.Context, owner, repo, ref string) ([]string, error)
}

// newDependabotLister builds the client used by `shuck dependabot`. It is a
// package var so tests can supply a stub without hitting GitHub.
var newDependabotLister = func(token string) dependabotLister { return gh.New(token) }

// DependabotOptions tunes a Dependabot audit.
type DependabotOptions struct {
	ConfigPath              string // explicit path to a dependabot config (overrides discovery)
	Ref                     string // git ref to fetch a remote config/tree from (default branch when empty)
	Token                   string
	PreferLocal             bool // scan the local checkout (files + .github/dependabot.yml) before the API
	ErrorOnMissingEcosystem bool // promote uncovered-ecosystem findings to errors
}

const dependabotUsage = `shuck dependabot — audit a repository's .github/dependabot.yml.

Usage:
  shuck dependabot                 the repo of the local working directory
  shuck dependabot <owner>/<repo>  an explicit repository
  shuck dependabot <url>           a github.com/<owner>/<repo>[/...] URL
  shuck dependabot discover [...]  scaffold or extend .github/dependabot.yml

shuck detects the package ecosystems the repository actually uses (from its
manifest files — go.mod, package.json, Dockerfile, *.tf, workflows, …) and
compares them with the update entries in .github/dependabot.yml. It reports
ecosystems that are used but not covered, plus best-practice gaps in each update
entry: missing groups, assignees, labels, cooldowns, open-PR limits, and
commit-message prefixes.

Ecosystem detection: for the local repo it scans the working directory; for an
explicit repo it lists the file tree via the GitHub API (use --ref to pick a
branch/tag/SHA). The config is read from the checkout for the local repo, or
fetched from the repo otherwise; --config overrides with a path.

Exit: 0 when a report is produced (even with findings), 2 on an operational
error. --exit-code exits 1 when there are error-level findings;
--error-on-missing-ecosystem makes an uncovered ecosystem an error; --strict
makes warnings gate too.

Auth: set GITHUB_TOKEN (or GH_TOKEN), or pass --token. Auditing the local repo
from its checkout needs no token; auditing an explicit repo reads its file tree
and config via the API.

Flags:
`

// runDependabot implements `shuck dependabot [owner/repo | url]` and dispatches
// the `discover` sub-subcommand.
func runDependabot(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "discover" {
		return runDependabotDiscover(args[1:], stdout, stderr)
	}

	fs := flag.NewFlagSet("shuck dependabot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		jsonOut          bool
		token            string
		config           string
		ref              string
		exitCode         bool
		strict           bool
		errOnMissingEcos bool
	)
	fs.BoolVar(&jsonOut, "json", false, "emit machine-readable JSON instead of the text summary")
	fs.StringVar(&token, "token", "", "GitHub token (overrides GITHUB_TOKEN/GH_TOKEN)")
	fs.StringVar(&config, "config", "", "path to the dependabot config (default: discover "+defaultDependabotConfig+")")
	fs.StringVar(&ref, "ref", "", "git ref to fetch a remote config/tree from (default: the repo's default branch)")
	fs.BoolVar(&exitCode, "exit-code", false, "exit 1 when there are error-level findings (for CI gating)")
	fs.BoolVar(&strict, "strict", false, "with --exit-code, treat warnings as errors too")
	fs.BoolVar(&errOnMissingEcos, "error-on-missing-ecosystem", false, "treat an ecosystem used but not in the config as an error")
	fs.Usage = func() {
		fmt.Fprint(stderr, dependabotUsage)
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

	report, err := Dependabot(context.Background(), owner, repo, DependabotOptions{
		ConfigPath:              config,
		Ref:                     ref,
		Token:                   token,
		PreferLocal:             len(fs.Args()) == 0,
		ErrorOnMissingEcosystem: errOnMissingEcos,
	})
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}

	if jsonOut {
		if err := dependabot.EncodeJSON(stdout, report); err != nil {
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
	} else {
		dependabot.Render(stdout, report)
	}

	return dependabotExit(report, exitCode, strict)
}

// dependabotExit maps a report to a process exit code. Producing a report is
// success (0); --exit-code flips error-level findings to 1, and --strict makes
// warnings gate as well.
func dependabotExit(r *model.DependabotReport, exitCode, strict bool) int {
	if !exitCode {
		return 0
	}
	if r.HasErrors() {
		return 1
	}
	if strict && r.Count(model.DependabotWarning) > 0 {
		return 1
	}
	return 0
}

// Dependabot audits a repository's Dependabot setup: it loads the config (if
// any), detects the ecosystems the repo uses, and evaluates the two into a
// report. It is exported so the MCP front-end shares the CLI's pipeline. A
// missing config is not fatal — it becomes a finding; an invalid config or an
// unreadable repository is an error.
func Dependabot(ctx context.Context, owner, repo string, opts DependabotOptions) (*model.DependabotReport, error) {
	token := opts.Token
	if token == "" {
		token = tokenFromEnv()
	}
	lister := newDependabotLister(token)

	data, source, hasConfig, misnamed, err := loadDependabotConfig(ctx, lister, owner, repo, opts)
	if err != nil {
		return nil, err
	}
	var cfg dependabot.Config
	if hasConfig {
		if cfg, err = dependabot.Parse(data); err != nil {
			return nil, err
		}
	}

	paths, err := repoFiles(ctx, lister, owner, repo, opts)
	if err != nil {
		return nil, err
	}

	return dependabot.Audit(dependabot.Input{
		Owner:                   owner,
		Repo:                    repo,
		ConfigSource:            source,
		HasConfig:               hasConfig,
		Config:                  cfg,
		Detected:                dependabot.Detect(paths),
		Misnamed:                misnamed,
		ErrorOnMissingEcosystem: opts.ErrorOnMissingEcosystem,
	}), nil
}

// loadDependabotConfig resolves the config bytes, a source label, whether a
// config exists at all, and whether it was found at the .yaml spelling GitHub
// ignores. A missing config is not an error here (it surfaces as a finding).
func loadDependabotConfig(ctx context.Context, lister dependabotLister, owner, repo string, opts DependabotOptions) (data []byte, source string, hasConfig, misnamed bool, err error) {
	if opts.ConfigPath != "" {
		data, err = os.ReadFile(opts.ConfigPath)
		if err != nil {
			return nil, "", false, false, fmt.Errorf("read --config %s: %w", opts.ConfigPath, err)
		}
		return data, opts.ConfigPath, true, false, nil
	}

	if opts.PreferLocal {
		for _, p := range []struct {
			path     string
			misnamed bool
		}{{defaultDependabotConfig, false}, {altDependabotConfig, true}} {
			data, err = os.ReadFile(p.path)
			if err == nil {
				return data, p.path, true, p.misnamed, nil
			}
			if !os.IsNotExist(err) {
				return nil, "", false, false, fmt.Errorf("read %s: %w", p.path, err)
			}
		}
		// Fall through to fetching it from the repo.
	}

	for _, p := range []struct {
		path     string
		misnamed bool
	}{{defaultDependabotConfig, false}, {altDependabotConfig, true}} {
		data, err = lister.FileContent(ctx, owner, repo, p.path, opts.Ref)
		if err == nil {
			return data, remoteConfigSource(owner, repo, opts.Ref, p.path), true, p.misnamed, nil
		}
		if !gh.FileNotFound(err) {
			return nil, "", false, false, err
		}
	}
	// No config anywhere — not fatal; the audit reports it as a finding.
	return nil, "", false, false, nil
}

// repoFiles lists the repository's file paths for ecosystem detection: the
// local working directory for the local repo, or the API file tree otherwise.
func repoFiles(ctx context.Context, lister dependabotLister, owner, repo string, opts DependabotOptions) ([]string, error) {
	if opts.PreferLocal {
		return scanLocalFiles(".")
	}
	return lister.RepoTree(ctx, owner, repo, opts.Ref)
}

// scanLocalFiles walks root and returns repo-relative, slash-separated file
// paths, skipping the heavy non-source directories Dependabot never scans.
func scanLocalFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && skipDir(d.Name()) {
				return iofs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan local files: %w", err)
	}
	return out, nil
}

// skipDir reports whether a directory should be pruned from the local scan.
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", ".cache":
		return true
	default:
		return false
	}
}

func remoteConfigSource(owner, repo, ref, path string) string {
	s := fmt.Sprintf("github:%s/%s", owner, repo)
	if ref != "" {
		s += "@" + ref
	}
	return s + ":" + path
}

const dependabotDiscoverUsage = `shuck dependabot discover — scaffold or extend .github/dependabot.yml.

Usage:
  shuck dependabot discover                 the repo of the local working directory
  shuck dependabot discover <owner>/<repo>  an explicit repository
  shuck dependabot discover <url>           a github.com/<owner>/<repo>[/...] URL

Detects the package ecosystems the repository uses and writes a best-practice
.github/dependabot.yml:

  - no config yet      a full config is scaffolded with a weekly schedule, a
                       minor/patch group, a label, an open-PR limit, and a
                       commit-message prefix for every detected ecosystem
  - config exists      an update entry is appended for each detected ecosystem
                       the config does not already cover (comments preserved)
  - up to date         nothing is written

Assignees are left out — shuck cannot know who should own the PRs — so add them
after generating. Exit: 0 on success, 2 on an operational error.

Flags:
`

// runDependabotDiscover implements `shuck dependabot discover [owner/repo | url]`.
func runDependabotDiscover(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck dependabot discover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		jsonOut bool
		token   string
		config  string
		ref     string
		dryRun  bool
	)
	fs.BoolVar(&jsonOut, "json", false, "emit machine-readable JSON instead of the text summary")
	fs.StringVar(&token, "token", "", "GitHub token (overrides GITHUB_TOKEN/GH_TOKEN)")
	fs.StringVar(&config, "config", "", "path of the dependabot config to create or extend (default: "+defaultDependabotConfig+")")
	fs.StringVar(&ref, "ref", "", "git ref to list a remote repo's tree from (default: the repo's default branch)")
	fs.BoolVar(&dryRun, "dry-run", false, "print the resulting config without writing it")
	fs.Usage = func() {
		fmt.Fprint(stderr, dependabotDiscoverUsage)
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

	disc, err := DependabotDiscover(context.Background(), owner, repo, DependabotDiscoverOptions{
		ConfigPath:  config,
		Ref:         ref,
		Token:       token,
		PreferLocal: len(fs.Args()) == 0,
		DryRun:      dryRun,
	})
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}

	if jsonOut {
		if err := dependabot.EncodeDiscoveryJSON(stdout, disc); err != nil {
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
		return 0
	}
	dependabot.RenderDiscovery(stdout, disc, dryRun)
	return 0
}

// DependabotDiscoverOptions tunes a Dependabot discovery.
type DependabotDiscoverOptions struct {
	ConfigPath  string // where to read/write the config (default: .github/dependabot.yml)
	Ref         string
	Token       string
	PreferLocal bool // scan the local working directory for ecosystems
	DryRun      bool // build the config but do not write it
}

// DependabotDiscover scaffolds or extends a local Dependabot config from the
// ecosystems a repository uses. It is exported so other front-ends can share
// the CLI's pipeline. The file is written unless DryRun is set (or it is already
// up to date).
func DependabotDiscover(ctx context.Context, owner, repo string, opts DependabotDiscoverOptions) (*dependabot.Discovery, error) {
	token := opts.Token
	if token == "" {
		token = tokenFromEnv()
	}
	lister := newDependabotLister(token)

	path := opts.ConfigPath
	if path == "" {
		path = defaultDependabotConfig
	}

	existing, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		existing = nil
	}
	// Validate an existing config up front so a typo'd key fails with a parse
	// error rather than being silently appended to.
	if existing != nil {
		if _, err = dependabot.Parse(existing); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	paths, err := repoFiles(ctx, lister, owner, repo, DependabotOptions{PreferLocal: opts.PreferLocal, Ref: opts.Ref})
	if err != nil {
		return nil, err
	}

	disc, err := dependabot.Discover(existing, dependabot.Detect(paths))
	if err != nil {
		return nil, err
	}
	disc.Owner, disc.Repo, disc.Path = owner, repo, path

	if !opts.DryRun && disc.Changed {
		if err := writeRepoConfig(path, disc.Data); err != nil {
			return nil, err
		}
	}
	return &disc, nil
}
