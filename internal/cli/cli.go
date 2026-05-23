// Package cli wires shuck's pipeline together: parse args, resolve the PR, read
// checks from GitHub (using the cache to avoid redundant log downloads), and
// render the failures.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/justanotherspy/shuck/internal/cache"
	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/jsonout"
	"github.com/justanotherspy/shuck/internal/logs"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/render"
	"github.com/justanotherspy/shuck/internal/target"
)

// version is the shuck version. It can be overridden at build time with
// -ldflags "-X github.com/justanotherspy/shuck/internal/cli.version=v1.2.3";
// otherwise it falls back to the module version baked in by `go install`.
var version = ""

// versionString reports the shuck version, preferring the ldflags-injected
// value and falling back to the Go build info (set when installed from a
// tagged module) or "dev" for plain `go build` from source.
func versionString() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

const usage = `shuck — show the exact failing CI step logs for a PR.

Usage:
  shuck <owner>/<repo> <pr>   inspect an explicit PR
  shuck <pr-url>              inspect a PR from its GitHub URL
  shuck <run-url>             inspect a single GitHub Actions run
  shuck <job-url>             inspect a single GitHub Actions job
  shuck <pr>                  inspect a PR (owner/repo from the local repo)
  shuck                       inspect the open PR for the current branch

Auth:
  Set GITHUB_TOKEN (or GH_TOKEN), or pass --token.

Flags:
`

type options struct {
	context        int
	shortThreshold int
	tail           int
	pattern        string
	full           bool
	token          string
	refresh        bool
	noCache        bool
	offline        bool
	json           bool
	version        bool
}

// Run executes shuck and returns the process exit code:
// 0 = no failing checks, 1 = failing checks reported, 2 = operational error.
func Run(args []string, stdout, stderr io.Writer) int {
	o, positional, err := parseArgs(args, stderr)
	if err != nil {
		return 2
	}

	if o.version {
		fmt.Fprintln(stdout, "shuck", versionString())
		return 0
	}

	exit, err := run(context.Background(), positional, o, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	return exit
}

// parseArgs defines shuck's flags, runs the arg-permutation pre-pass, and parses
// args into the options and the leftover positional target tokens. Splitting it
// out of Run keeps the real flag set (not a test copy) exercisable in tests.
func parseArgs(args []string, stderr io.Writer) (options, []string, error) {
	fs := flag.NewFlagSet("shuck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}

	var o options
	fs.IntVar(&o.context, "context", 10, "lines of context kept around each error match")
	fs.IntVar(&o.shortThreshold, "short-threshold", 100, "logs with at most this many lines are shown whole")
	fs.IntVar(&o.tail, "tail", 100, "lines tailed when a long log has no error match")
	fs.StringVar(&o.pattern, "pattern", "", "override the error-matching regexp")
	fs.BoolVar(&o.full, "full", false, "show full, untrimmed logs for failed steps")
	fs.StringVar(&o.token, "token", "", "GitHub token (overrides GITHUB_TOKEN/GH_TOKEN)")
	fs.BoolVar(&o.refresh, "refresh", false, "ignore and rebuild the cache")
	fs.BoolVar(&o.noCache, "no-cache", false, "do not read or write the cache")
	fs.BoolVar(&o.offline, "offline", false, "render only from cache, without network access")
	fs.BoolVar(&o.json, "json", false, "emit machine-readable JSON (stable schema) instead of text")
	fs.BoolVar(&o.version, "version", false, "print the shuck version and exit")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return options{}, nil, err
	}
	return o, fs.Args(), nil
}

func run(ctx context.Context, args []string, o options, stdout io.Writer) (int, error) {
	extractOpts, err := buildExtractOptions(o)
	if err != nil {
		return 0, err
	}

	tgt, err := target.Resolve(args)
	if err != nil {
		return 0, err
	}

	if o.offline {
		if tgt.RunID != 0 {
			return 0, fmt.Errorf("--offline is not supported for run/job URLs; it works only with PR targets, which are cached")
		}
		return runOffline(tgt, o.json, stdout)
	}

	token, err := resolveToken(o.token)
	if err != nil {
		return 0, err
	}
	client := gh.New(token)
	a := &app{client: client, opts: extractOpts}

	if tgt.RunID != 0 {
		return a.inspectRun(ctx, tgt, o.json, stdout)
	}

	number := tgt.Number
	if number == 0 {
		number, err = client.FindOpenPR(ctx, tgt.Owner, tgt.Repo, tgt.Owner, tgt.Branch)
		if err != nil {
			return 0, err
		}
	}

	pr, err := client.GetPR(ctx, tgt.Owner, tgt.Repo, number)
	if err != nil {
		return 0, err
	}

	var cached *model.Report
	if !o.refresh && !o.noCache {
		if cached, err = cache.Load(tgt.Owner, tgt.Repo, number); err != nil {
			return 0, err
		}
	}

	failed, cancelled, running, err := client.ListJobs(ctx, tgt.Owner, tgt.Repo, pr.HeadSHA)
	if err != nil {
		return 0, err
	}
	other, err := client.OtherChecks(ctx, tgt.Owner, tgt.Repo, pr.HeadSHA)
	if err != nil {
		return 0, err
	}

	var reuseFrom *model.Report
	if cached != nil && cached.PR.HeadSHA == pr.HeadSHA {
		reuseFrom = cached
	}
	inspected := cache.InspectedJobs(reuseFrom)

	for i := range failed {
		key := cache.JobKey{ID: failed[i].ID, RunAttempt: failed[i].RunAttempt}
		if prev, ok := inspected[key]; ok {
			failed[i].FailedSteps = prev.FailedSteps
			failed[i].Inspected = true
			continue
		}
		a.drill(ctx, tgt.Owner, tgt.Repo, &failed[i])
	}

	report := &model.Report{
		PR:            pr,
		FailedJobs:    failed,
		CancelledJobs: cancelled,
		RunningJobs:   running,
		OtherChecks:   other,
		CheckedAt:     time.Now(),
	}

	if !o.noCache {
		if err := cache.Save(report); err != nil {
			fmt.Fprintln(os.Stderr, "shuck: warning: could not write cache:", err)
		}
	}

	return emit(stdout, report, o.json)
}

func runOffline(tgt target.Target, jsonOut bool, stdout io.Writer) (int, error) {
	if tgt.Number == 0 {
		return 0, fmt.Errorf("--offline requires an explicit PR number")
	}
	cached, err := cache.Load(tgt.Owner, tgt.Repo, tgt.Number)
	if err != nil {
		return 0, err
	}
	if cached == nil {
		return 0, fmt.Errorf("no cache for %s/%s#%d; run online first", tgt.Owner, tgt.Repo, tgt.Number)
	}
	return emit(stdout, cached, jsonOut)
}

// inspectRun handles a run/job URL target: it fetches the run's jobs (or a
// single job), drills the failed ones for their error logs, and renders. Run
// targets bypass the PR-keyed cache, so logs are always re-downloaded.
func (a *app) inspectRun(ctx context.Context, tgt target.Target, jsonOut bool, stdout io.Writer) (int, error) {
	info, failed, cancelled, running, err := a.client.RunReport(ctx, tgt.Owner, tgt.Repo, tgt.RunID, tgt.JobID)
	if err != nil {
		return 0, err
	}
	for i := range failed {
		a.drill(ctx, tgt.Owner, tgt.Repo, &failed[i])
	}
	report := &model.Report{
		Run:           &info,
		FailedJobs:    failed,
		CancelledJobs: cancelled,
		RunningJobs:   running,
		CheckedAt:     time.Now(),
	}
	return emit(stdout, report, jsonOut)
}

// emit renders the report as JSON or human-readable text and returns the
// process exit code for its failure state.
func emit(stdout io.Writer, report *model.Report, jsonOut bool) (int, error) {
	if jsonOut {
		if err := jsonout.Encode(stdout, report); err != nil {
			return 0, err
		}
		return exitFor(report), nil
	}
	render.Report(stdout, report)
	return exitFor(report), nil
}

// permuteArgs reorders args so flags may appear after positional arguments. Go's
// flag package stops at the first non-flag token, so "shuck owner/repo 42 --json"
// would otherwise treat --json as a positional. Everything after a literal "--"
// is preserved verbatim as positional. A "--" separator is always emitted so
// flag.Parse terminates flag scanning exactly where the positionals begin.
//
// Each token's leading dashes are first run through canonicalDashes so a flag
// typed with a Unicode dash ("—full", from macOS smart dashes or rich-text
// copy-paste) is recognized as a flag rather than mistaken for a positional —
// otherwise "shuck 42 —full" would see two positionals and fail with the
// misleading "invalid repo" error. Flag values are passed through verbatim.
func permuteArgs(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := canonicalDashes(args[i])
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && flagTakesValue(fs, a) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, args[i])
	}
	out := append(flags, "--")
	return append(out, positional...)
}

// canonicalDashes rewrites a leading run of Unicode dash characters into the
// ASCII hyphens the user meant, so "—full" becomes "--full". This recovers
// flags mangled by macOS "smart dashes" (which turn "--" into an em-dash) or by
// copy-pasting a command out of rich text. Only a leading run is touched, so
// every positional target form (a number, owner/repo, or a URL — none of which
// begin with a dash) is returned unchanged.
func canonicalDashes(arg string) string {
	runes := []rune(arg)
	var b strings.Builder
	i := 0
	for i < len(runes) {
		repl, ok := dashReplacement(runes[i])
		if !ok {
			break
		}
		b.WriteString(repl)
		i++
	}
	if i == 0 {
		return arg
	}
	b.WriteString(string(runes[i:]))
	return b.String()
}

// dashReplacement maps a dash-like rune to the ASCII hyphens it stands in for:
// the wide dashes (en/em dash, horizontal bar) replace "--", while ASCII '-' and
// the hyphen-width variants replace a single "-". Go's flag package treats "-x"
// and "--x" identically, so the single/double choice only matters for keeping a
// lone wide dash as the "--" positional separator. ok is false for non-dashes.
func dashReplacement(r rune) (string, bool) {
	switch r {
	case '-',
		'‐', // hyphen
		'‑', // non-breaking hyphen
		'‒', // figure dash
		'−': // minus sign
		return "-", true
	case '–', // en dash
		'—', // em dash
		'―': // horizontal bar
		return "--", true
	}
	return "", false
}

// flagTakesValue reports whether arg names a defined non-boolean flag, which
// consumes the following token as its value. Unknown flags return false so
// flag.Parse surfaces the error instead of this swallowing the next argument.
func flagTakesValue(fs *flag.FlagSet, arg string) bool {
	name := strings.TrimLeft(arg, "-")
	if eq := strings.IndexByte(name, '='); eq >= 0 {
		name = name[:eq]
	}
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !bf.IsBoolFlag()
}

// app holds the dependencies needed to drill into a failed job's logs.
type app struct {
	client *gh.Client
	opts   logs.Options
}

func (a *app) drill(ctx context.Context, owner, repo string, job *model.JobResult) {
	job.Inspected = true
	raw, err := a.client.JobLog(ctx, owner, repo, job.ID)
	if err != nil {
		job.FailedSteps = []model.FailedStep{{
			Name:    "(logs unavailable)",
			Excerpt: fmt.Sprintf("could not download logs: %v", err),
		}}
		return
	}
	job.FailedSteps = a.buildFailedSteps(*job, raw)
}

// buildFailedSteps pairs the API's failed steps with the log's error-bearing
// sections (by order) to recover each failed step's command and error excerpt.
func (a *app) buildFailedSteps(job model.JobResult, raw string) []model.FailedStep {
	sections := logs.Parse(raw)
	errSecs := logs.ErrorSections(sections)

	var failedSteps []model.StepOverview
	for _, s := range job.Steps {
		if model.IsFailureConclusion(s.Conclusion) {
			failedSteps = append(failedSteps, s)
		}
	}

	if len(errSecs) == 0 {
		var all []string
		for _, sec := range sections {
			all = append(all, sec.Body...)
		}
		fs := model.FailedStep{Name: "(job log)", Excerpt: logs.Extract(all, a.opts)}
		if len(failedSteps) > 0 {
			fs.Name = failedSteps[0].Name
			fs.Number = failedSteps[0].Number
		}
		return []model.FailedStep{fs}
	}

	n := len(failedSteps)
	if len(errSecs) > n {
		n = len(errSecs)
	}
	out := make([]model.FailedStep, 0, n)
	for i := 0; i < n; i++ {
		fs := model.FailedStep{Name: "(unnamed step)"}
		if i < len(failedSteps) {
			fs.Number = failedSteps[i].Number
			fs.Name = failedSteps[i].Name
		}
		if i < len(errSecs) {
			sec := errSecs[i]
			fs.Command = sec.Command()
			fs.Kind = sec.Kind()
			fs.Excerpt = logs.Extract(sec.Body, a.opts)
		} else {
			fs.Excerpt = "(no matching error log section found)"
		}
		out = append(out, fs)
	}
	return out
}

func buildExtractOptions(o options) (logs.Options, error) {
	for _, f := range []struct {
		name string
		val  int
	}{
		{"--context", o.context},
		{"--short-threshold", o.shortThreshold},
		{"--tail", o.tail},
	} {
		if f.val < 0 {
			return logs.Options{}, fmt.Errorf("%s must be non-negative, got %d", f.name, f.val)
		}
	}
	opts := logs.Options{
		ShortThreshold: o.shortThreshold,
		Context:        o.context,
		Tail:           o.tail,
		Pattern:        logs.DefaultPattern(),
	}
	if o.pattern != "" {
		re, err := regexp.Compile(o.pattern)
		if err != nil {
			return logs.Options{}, fmt.Errorf("invalid --pattern: %w", err)
		}
		opts.Pattern = re
	}
	if o.full {
		opts.ShortThreshold = math.MaxInt
	}
	return opts, nil
}

func resolveToken(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("no GitHub token found: set GITHUB_TOKEN (or GH_TOKEN), or pass --token")
}

func exitFor(r *model.Report) int {
	if r.HasFailures() {
		return 1
	}
	return 0
}
