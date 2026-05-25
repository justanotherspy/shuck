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
	"os/signal"
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
  shuck --watch [target]      poll until every check finishes, then print the report
  shuck mcp                   run as a local MCP (stdio) server exposing shuck tools
  shuck setup                 install the shuck skill + CLAUDE.md note for Claude Code (and, optionally, the MCP)
  shuck version [--check]     print the installed version; --check looks for a newer release
  shuck upgrade               download and install the latest release in place

Auth:
  Set GITHUB_TOKEN (or GH_TOKEN), or pass --token.

Flags:
`

type options struct {
	context         int
	shortThreshold  int
	tail            int
	pattern         string
	full            bool
	maxCommandLines int
	token           string
	refresh         bool
	noCache         bool
	offline         bool
	json            bool
	version         bool
	watch           bool
	interval        time.Duration
	watchTimeout    time.Duration
}

// Run executes shuck and returns the process exit code:
// 0 = no failing checks, 1 = failing checks reported, 2 = operational error.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "version":
			return runVersion(args[1:], stdout, stderr)
		case "upgrade":
			return runUpgrade(args[1:], stdout, stderr)
		}
	}

	o, positional, err := parseArgs(args, stderr)
	if err != nil {
		return 2
	}

	if o.version {
		fmt.Fprintln(stdout, "shuck", versionString())
		return 0
	}

	ctx := context.Background()
	if o.watch {
		// In watch mode we block for a long time, so honour Ctrl-C: cancelling
		// the context stops the poll loop and prints the latest result.
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
	}

	exit, err := run(ctx, positional, o, stdout, stderr)
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
	fs.IntVar(&o.maxCommandLines, "max-command-lines", logs.DefaultMaxCommandLines, "max lines of a failed step's command to show (0 = no limit)")
	fs.StringVar(&o.token, "token", "", "GitHub token (overrides GITHUB_TOKEN/GH_TOKEN)")
	fs.BoolVar(&o.refresh, "refresh", false, "ignore and rebuild the cache")
	fs.BoolVar(&o.noCache, "no-cache", false, "do not read or write the cache")
	fs.BoolVar(&o.offline, "offline", false, "render only from cache, without network access")
	fs.BoolVar(&o.json, "json", false, "emit machine-readable JSON (stable schema) instead of text")
	fs.BoolVar(&o.version, "version", false, "print the shuck version and exit")
	fs.BoolVar(&o.watch, "watch", false, "poll until every check reaches a terminal state, then print the report")
	fs.DurationVar(&o.interval, "interval", 15*time.Second, "poll interval for --watch")
	fs.DurationVar(&o.watchTimeout, "watch-timeout", 0, "give up watching after this long (0 = no limit)")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return options{}, nil, err
	}
	return o, fs.Args(), nil
}

func run(ctx context.Context, args []string, o options, stdout, stderr io.Writer) (int, error) {
	tgt, err := target.Resolve(args)
	if err != nil {
		return 0, err
	}
	if o.watch {
		return runWatch(ctx, tgt, o, stdout, stderr)
	}
	report, err := inspectWith(ctx, tgt, o)
	if err != nil {
		return 0, err
	}
	return emit(stdout, report, o.json)
}

// runWatch validates the watch knobs and drives the poll loop for tgt, wiring
// in the real inspection and a context-aware sleep.
func runWatch(ctx context.Context, tgt target.Target, o options, stdout, stderr io.Writer) (int, error) {
	if o.offline {
		return 0, fmt.Errorf("--watch cannot be combined with --offline: the cache does not change while you wait")
	}
	if o.interval <= 0 {
		return 0, fmt.Errorf("--interval must be positive, got %s", o.interval)
	}
	if o.watchTimeout < 0 {
		return 0, fmt.Errorf("--watch-timeout must not be negative, got %s", o.watchTimeout)
	}
	inspect := func(ctx context.Context) (*model.Report, error) {
		return inspectWith(ctx, tgt, o)
	}
	return watch(ctx, o, inspect, sleepCtx, stdout, stderr)
}

// watch polls inspect until the report is terminal (no jobs still running),
// the watch-timeout elapses, or ctx is cancelled, then emits the latest report.
// inspect and sleep are injected so the loop's termination logic is testable
// without network or real waiting. sleep reports false when ctx was cancelled.
func watch(
	ctx context.Context,
	o options,
	inspect func(context.Context) (*model.Report, error),
	sleep func(context.Context, time.Duration) bool,
	stdout, stderr io.Writer,
) (int, error) {
	var deadline time.Time
	if o.watchTimeout > 0 {
		deadline = time.Now().Add(o.watchTimeout)
	}

	for {
		report, err := inspect(ctx)
		if err != nil {
			return 0, err
		}
		if report.IsTerminal() {
			return emit(stdout, report, o.json)
		}

		wait := o.interval
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				fmt.Fprintf(stderr, "shuck: gave up watching after %s with %d job(s) still running\n",
					o.watchTimeout, len(report.RunningJobs))
				return emit(stdout, report, o.json)
			}
			if remaining < wait {
				wait = remaining
			}
		}

		fmt.Fprintf(stderr, "shuck: %d running, %d failed so far — re-checking in %s\n",
			len(report.RunningJobs), len(report.FailedJobs), wait.Round(time.Second))

		if !sleep(ctx, wait) {
			fmt.Fprintln(stderr, "shuck: stopped watching — printing the latest result")
			return emit(stdout, report, o.json)
		}
	}
}

// sleepCtx waits for d or until ctx is cancelled, reporting true if the full
// duration elapsed and false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// InspectOptions controls a single inspection: the log-extraction tuning that
// mirrors the CLI flags plus the cache behavior. It is the front-end-agnostic
// input to [Inspect], used by alternative entry points such as the MCP server.
type InspectOptions struct {
	Context         int
	ShortThreshold  int
	Tail            int
	Pattern         string
	Full            bool
	MaxCommandLines int
	Token           string
	Refresh         bool
	NoCache         bool
	Offline         bool
}

// Inspect runs shuck's pipeline for an already-resolved target and returns the
// report without rendering it. It is the reusable core behind the CLI and the
// MCP server: callers decide how to present the result (text, JSON, or a
// structured tool response).
func Inspect(ctx context.Context, tgt target.Target, opts InspectOptions) (*model.Report, error) {
	return inspectWith(ctx, tgt, options{
		context:         opts.Context,
		shortThreshold:  opts.ShortThreshold,
		tail:            opts.Tail,
		pattern:         opts.Pattern,
		full:            opts.Full,
		maxCommandLines: opts.MaxCommandLines,
		token:           opts.Token,
		refresh:         opts.Refresh,
		noCache:         opts.NoCache,
		offline:         opts.Offline,
	})
}

// Version reports the shuck version for non-CLI front-ends (e.g. the MCP
// server advertises it in its server info).
func Version() string { return versionString() }

// inspectWith builds the report for a resolved target: it validates the
// extraction options, then dispatches to the offline, run, or PR path.
func inspectWith(ctx context.Context, tgt target.Target, o options) (*model.Report, error) {
	extractOpts, err := buildExtractOptions(o)
	if err != nil {
		return nil, err
	}
	if o.maxCommandLines < 0 {
		return nil, fmt.Errorf("--max-command-lines must be non-negative, got %d", o.maxCommandLines)
	}

	if o.offline {
		if tgt.RunID != 0 {
			return nil, fmt.Errorf("offline is not supported for run/job URLs; it works only with PR targets, which are cached")
		}
		return loadOffline(tgt)
	}

	token, err := resolveToken(o.token)
	if err != nil {
		return nil, err
	}
	a := &app{client: gh.New(token), opts: extractOpts, maxCommandLines: o.maxCommandLines}

	if tgt.RunID != 0 {
		return a.runReport(ctx, tgt)
	}
	return a.prReport(ctx, tgt, o.refresh, o.noCache)
}

// prReport resolves and drills a PR target, reusing cached per-job log detail
// for job attempts already inspected on the same head commit.
func (a *app) prReport(ctx context.Context, tgt target.Target, refresh, noCache bool) (*model.Report, error) {
	number := tgt.Number
	if number == 0 {
		var err error
		number, err = a.client.FindOpenPR(ctx, tgt.Owner, tgt.Repo, tgt.Owner, tgt.Branch)
		if err != nil {
			return nil, err
		}
	}

	pr, err := a.client.GetPR(ctx, tgt.Owner, tgt.Repo, number)
	if err != nil {
		return nil, err
	}

	var cached *model.Report
	if !refresh && !noCache {
		if cached, err = cache.Load(tgt.Owner, tgt.Repo, number); err != nil {
			return nil, err
		}
	}

	failed, cancelled, running, err := a.client.ListJobs(ctx, tgt.Owner, tgt.Repo, pr.HeadSHA)
	if err != nil {
		return nil, err
	}
	other, err := a.client.OtherChecks(ctx, tgt.Owner, tgt.Repo, pr.HeadSHA)
	if err != nil {
		return nil, err
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

	if !noCache {
		if err := cache.Save(report); err != nil {
			fmt.Fprintln(os.Stderr, "shuck: warning: could not write cache:", err)
		}
	}
	return report, nil
}

// loadOffline renders a PR target only from its cache, without network access.
func loadOffline(tgt target.Target) (*model.Report, error) {
	if tgt.Number == 0 {
		return nil, fmt.Errorf("offline requires an explicit PR number")
	}
	cached, err := cache.Load(tgt.Owner, tgt.Repo, tgt.Number)
	if err != nil {
		return nil, err
	}
	if cached == nil {
		return nil, fmt.Errorf("no cache for %s/%s#%d; run online first", tgt.Owner, tgt.Repo, tgt.Number)
	}
	return cached, nil
}

// runReport handles a run/job URL target: it fetches the run's jobs (or a
// single job) and drills the failed ones for their error logs. Run targets
// bypass the PR-keyed cache, so logs are always re-downloaded.
func (a *app) runReport(ctx context.Context, tgt target.Target) (*model.Report, error) {
	info, failed, cancelled, running, err := a.client.RunReport(ctx, tgt.Owner, tgt.Repo, tgt.RunID, tgt.JobID)
	if err != nil {
		return nil, err
	}
	for i := range failed {
		a.drill(ctx, tgt.Owner, tgt.Repo, &failed[i])
	}
	return &model.Report{
		Run:           &info,
		FailedJobs:    failed,
		CancelledJobs: cancelled,
		RunningJobs:   running,
		CheckedAt:     time.Now(),
	}, nil
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
	client          *gh.Client
	opts            logs.Options
	maxCommandLines int
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
			fs.Command = logs.ClampCommand(sec.FullCommand(), a.maxCommandLines)
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
