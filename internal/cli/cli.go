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

const usage = `shuck — show the exact failing CI step logs, reviews, and security alerts for a PR.

By default (and via "shuck all") shuck reports a PR's failing CI, its reviews,
and the repo's security alerts together. Use a subcommand to focus on one.

Usage:
  shuck [target]              CI + reviews + security for a PR (same as "shuck all")
  shuck <owner>/<repo> <pr>   an explicit PR ("shuck <pr-url>", "shuck <pr>", or "shuck" for the current branch)
  shuck <run-url> | <job-url> a single GitHub Actions run / job (CI only)
  shuck --watch [target]      poll until every check finishes, then print the report

Subcommands (single-letter shorthands in parentheses):
  shuck logs (l) [target] [--run <id|url>]   failing CI step logs for a PR or a single run
  shuck reviews (r) [target]                 a PR's reviews and review-comment threads
  shuck all [target]                         CI + reviews + security (the default)
  shuck action (a) <owner>/<action>[@<version>]  resolve an Action to its latest tag + SHA for pinning
  shuck image (i) [owner | ghcr.io/owner/name[:tag]]  list GHCR images, or resolve one to its latest digest
  shuck security (s) [owner/repo | url]      a repo's security alerts (code scanning, secrets, Dependabot)
  shuck compliance (c) [owner/repo | url]    check a repo's settings against its .shuck/compliance.yaml
  shuck mcp                   run as a local MCP (stdio) server exposing shuck tools
  shuck setup                 install the shuck skill + CLAUDE.md note for Claude Code (and, optionally, the MCP)
  shuck version [--check]     print the installed version; --check looks for a newer release
  shuck upgrade               download and install the latest release in place

Auth:
  Set GITHUB_TOKEN (or GH_TOKEN), or pass --token.

Flags:
`

type options struct {
	context            int
	shortThreshold     int
	tail               int
	pattern            string
	full               bool
	maxCommandLines    int
	reviewCommentLimit int
	ciOnly             bool
	reviewsOnly        bool
	state              string
	token              string
	refresh            bool
	noCache            bool
	offline            bool
	json               bool
	version            bool
	watch              bool
	interval           time.Duration
	watchTimeout       time.Duration
}

// subcommandAliases maps single-letter shorthands to their canonical
// subcommand name dispatched in Run. `all` has no shorthand: `a` is `action`.
var subcommandAliases = map[string]string{
	"l": "logs",
	"r": "reviews",
	"a": "action",
	"s": "security",
	"c": "compliance",
	"i": "image",
}

// Run executes shuck and returns the process exit code:
// 0 = no failing checks, 1 = failing checks reported, 2 = operational error.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		cmd := args[0]
		if canon, ok := subcommandAliases[cmd]; ok {
			cmd = canon
		}
		switch cmd {
		case "version":
			return runVersion(args[1:], stdout, stderr)
		case "upgrade":
			return runUpgrade(args[1:], stdout, stderr)
		case "action":
			return runAction(args[1:], stdout, stderr)
		case "image", "images":
			return runImage(args[1:], stdout, stderr)
		case "security":
			return runSecurity(args[1:], stdout, stderr)
		case "compliance":
			return runCompliance(args[1:], stdout, stderr)
		case "logs":
			return runLogs(args[1:], stdout, stderr)
		case "reviews":
			return runReviews(args[1:], stdout, stderr)
		case "all":
			return runDefault(args[1:], stdout, stderr)
		}
	}
	return runDefault(args, stdout, stderr)
}

// runDefault is the bare `shuck [target]` path (also reached via `shuck all`):
// it parses the full flag set and runs the combined CI + reviews + security
// report for a PR target (CI-only for run/job targets).
func runDefault(args []string, stdout, stderr io.Writer) int {
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
		// In watch mode we block for a long time, so honor Ctrl-C: canceling
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
	registerInspectFlags(fs, &o)
	fs.StringVar(&o.state, "state", "open", "security alert state to include: open|all|dismissed|fixed|resolved")
	fs.BoolVar(&o.version, "version", false, "print the shuck version and exit")
	fs.BoolVar(&o.watch, "watch", false, "poll until every check reaches a terminal state, then print the report")
	fs.DurationVar(&o.interval, "interval", 15*time.Second, "poll interval for --watch")
	fs.DurationVar(&o.watchTimeout, "watch-timeout", 0, "give up watching after this long (0 = no limit)")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return options{}, nil, err
	}
	return o, fs.Args(), nil
}

// registerInspectFlags registers the extraction, cache, output, and auth flags
// shared by the default path and the `logs` / `reviews` subcommands, so their
// definitions never drift. Focus (ci-only / reviews-only) is set internally by
// each subcommand, not via a flag.
func registerInspectFlags(fs *flag.FlagSet, o *options) {
	fs.IntVar(&o.context, "context", 10, "lines of context kept around each error match")
	fs.IntVar(&o.shortThreshold, "short-threshold", 100, "logs with at most this many lines are shown whole")
	fs.IntVar(&o.tail, "tail", 100, "lines tailed when a long log has no error match")
	fs.StringVar(&o.pattern, "pattern", "", "override the error-matching regexp")
	fs.BoolVar(&o.full, "full", false, "show full, untrimmed logs for failed steps")
	fs.IntVar(&o.maxCommandLines, "max-command-lines", logs.DefaultMaxCommandLines, "max lines of a failed step's command to show (0 = no limit)")
	fs.IntVar(&o.reviewCommentLimit, "review-comment-limit", 5, "max comments shown per active review thread")
	fs.StringVar(&o.token, "token", "", "GitHub token (overrides GITHUB_TOKEN/GH_TOKEN)")
	fs.BoolVar(&o.refresh, "refresh", false, "ignore and rebuild the cache")
	fs.BoolVar(&o.noCache, "no-cache", false, "do not read or write the cache")
	fs.BoolVar(&o.offline, "offline", false, "render only from cache, without network access")
	fs.BoolVar(&o.json, "json", false, "emit machine-readable JSON (stable schema) instead of text")
}

func run(ctx context.Context, args []string, o options, stdout, stderr io.Writer) (int, error) {
	tgt, err := target.Resolve(args)
	if err != nil {
		return 0, err
	}
	if o.watch {
		return runWatch(ctx, tgt, o, stdout, stderr)
	}
	res, err := inspectAll(ctx, tgt, o)
	if err != nil {
		return 0, err
	}
	return emitAll(stdout, res, o.json)
}

// runWatch validates the watch knobs and drives the poll loop for tgt, wiring
// in the real inspection, a context-aware sleep, and the combined emit (the
// security half is fetched once, at the terminal poll).
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
	emitFn := func(report *model.Report) (int, error) {
		return emitAll(stdout, withSecurity(ctx, tgt, o, report), o.json)
	}
	return watch(ctx, o, inspect, sleepCtx, emitFn, stdout, stderr)
}

// watch polls inspect until the report is terminal (no jobs still running),
// the watch-timeout elapses, or ctx is cancelled, then emits the latest report
// via emit. inspect, sleep, and emit are injected so the loop's termination
// logic is testable without network or real waiting. sleep reports false when
// ctx was cancelled.
func watch(
	ctx context.Context,
	o options,
	inspect func(context.Context) (*model.Report, error),
	sleep func(context.Context, time.Duration) bool,
	emit func(*model.Report) (int, error),
	_, stderr io.Writer,
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
			return emit(report)
		}

		wait := o.interval
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				fmt.Fprintf(stderr, "shuck: gave up watching after %s with %d job(s) still running\n",
					o.watchTimeout, len(report.RunningJobs))
				return emit(report)
			}
			if remaining < wait {
				wait = remaining
			}
		}

		fmt.Fprintf(stderr, "shuck: %d running, %d failed so far — re-checking in %s\n",
			len(report.RunningJobs), len(report.FailedJobs), wait.Round(time.Second))

		if !sleep(ctx, wait) {
			fmt.Fprintln(stderr, "shuck: stopped watching — printing the latest result")
			return emit(report)
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
	Context            int
	ShortThreshold     int
	Tail               int
	Pattern            string
	Full               bool
	MaxCommandLines    int
	ReviewCommentLimit int
	CIOnly             bool
	ReviewsOnly        bool
	Token              string
	Refresh            bool
	NoCache            bool
	Offline            bool
}

// Inspect runs shuck's pipeline for an already-resolved target and returns the
// report without rendering it. It is the reusable core behind the CLI and the
// MCP server: callers decide how to present the result (text, JSON, or a
// structured tool response).
func Inspect(ctx context.Context, tgt target.Target, opts InspectOptions) (*model.Report, error) {
	return inspectWith(ctx, tgt, options{
		context:            opts.Context,
		shortThreshold:     opts.ShortThreshold,
		tail:               opts.Tail,
		pattern:            opts.Pattern,
		full:               opts.Full,
		maxCommandLines:    opts.MaxCommandLines,
		reviewCommentLimit: opts.ReviewCommentLimit,
		ciOnly:             opts.CIOnly,
		reviewsOnly:        opts.ReviewsOnly,
		token:              opts.Token,
		refresh:            opts.Refresh,
		noCache:            opts.NoCache,
		offline:            opts.Offline,
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
	if o.reviewCommentLimit < 1 {
		return nil, fmt.Errorf("--review-comment-limit must be at least 1, got %d", o.reviewCommentLimit)
	}

	if o.offline {
		if tgt.RunID != 0 {
			return nil, fmt.Errorf("offline is not supported for run/job URLs; it works only with PR targets, which are cached")
		}
		report, err := loadOffline(tgt)
		if err != nil {
			return nil, err
		}
		applyFocus(report, o.ciOnly, o.reviewsOnly)
		return report, nil
	}

	token, err := resolveToken(o.token)
	if err != nil {
		return nil, err
	}
	a := &app{
		client:             gh.New(token),
		opts:               extractOpts,
		maxCommandLines:    o.maxCommandLines,
		reviewCommentLimit: o.reviewCommentLimit,
		ciOnly:             o.ciOnly,
		reviewsOnly:        o.reviewsOnly,
	}

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

	if dir, err := cache.Dir(tgt.Owner, tgt.Repo, number); err == nil {
		_ = cache.Purge(time.Hour, dir)
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

	report := &model.Report{PR: pr, ReviewsOnly: a.reviewsOnly, CheckedAt: time.Now()}

	if !a.reviewsOnly {
		failed, cancelled, running, err := a.client.ListJobs(ctx, tgt.Owner, tgt.Repo, pr.HeadSHA)
		if err != nil {
			return nil, err
		}
		other, err := a.client.OtherChecks(ctx, tgt.Owner, tgt.Repo, pr.HeadSHA)
		if err != nil {
			return nil, err
		}

		// Completed-job logs are immutable, so on the same head commit we reuse a
		// cached raw log and re-parse it under the *current* flags (--full,
		// --context, --pattern, …) rather than copying stale excerpts. Newly
		// finished attempts are drilled and their raw log cached for next time.
		// Cancelled jobs are drilled too: their log shows the step that was
		// running (and its output) when the job was interrupted.
		sameCommit := !refresh && !noCache && cached != nil && cached.PR.HeadSHA == pr.HeadSHA
		a.drillJobs(ctx, tgt.Owner, tgt.Repo, number, failed, sameCommit, noCache)
		a.drillJobs(ctx, tgt.Owner, tgt.Repo, number, cancelled, sameCommit, noCache)

		report.FailedJobs = failed
		report.CancelledJobs = cancelled
		report.RunningJobs = running
		report.OtherChecks = other
	}

	if !a.ciOnly {
		a.attachReviews(ctx, tgt.Owner, tgt.Repo, number, refresh, cached, report)
	}

	if !noCache {
		// Focus modes (`logs` / `reviews`) render one dimension but persist the
		// other from the existing cache so neither subcommand clobbers the other's
		// data. The rendered report is left untouched.
		toSave := *report
		if a.reviewsOnly && cached != nil {
			toSave.FailedJobs, toSave.CancelledJobs, toSave.RunningJobs = cached.FailedJobs, cached.CancelledJobs, cached.RunningJobs
			toSave.OtherChecks, toSave.Run = cached.OtherChecks, cached.Run
		}
		if a.ciOnly && cached != nil {
			toSave.Reviews, toSave.ReviewsFingerprint = cached.Reviews, cached.ReviewsFingerprint
		}
		if err := cache.Save(&toSave); err != nil {
			fmt.Fprintln(os.Stderr, "shuck: warning: could not write cache:", err)
		}
	}
	return report, nil
}

// attachReviews fills report.Reviews, reusing the cached reviews when a cheap
// fingerprint shows the PR's review state is unchanged. Review fetching is
// best-effort: any error is reported as a warning and leaves reviews empty
// rather than failing the whole inspection.
func (a *app) attachReviews(ctx context.Context, owner, repo string, number int, refresh bool, cached, report *model.Report) {
	fingerprint, err := a.client.ReviewsFingerprint(ctx, owner, repo, number)
	if err != nil {
		fmt.Fprintln(os.Stderr, "shuck: warning: could not check reviews:", err)
		return
	}
	if !refresh && cached != nil && cached.ReviewsFingerprint == fingerprint && len(cached.Reviews) > 0 {
		report.Reviews = cached.Reviews
		report.ReviewsFingerprint = fingerprint
		return
	}
	reviews, err := a.client.PRReviews(ctx, owner, repo, number, a.reviewCommentLimit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "shuck: warning: could not fetch reviews:", err)
		return
	}
	report.Reviews = reviews
	report.ReviewsFingerprint = fingerprint
}

// applyFocus narrows a report to a focus subcommand's single dimension, matching
// what the online `logs` / `reviews` paths produce (each fetches only one half).
// The offline path loads the whole cached report — which carries both halves —
// so without this `shuck logs --offline` would also print cached reviews and
// `shuck reviews --offline` would print (and exit-code on) cached CI failures.
func applyFocus(report *model.Report, ciOnly, reviewsOnly bool) {
	if ciOnly {
		report.Reviews = nil
		report.ReviewsFingerprint = ""
	}
	if reviewsOnly {
		report.ReviewsOnly = true
		report.FailedJobs = nil
		report.CancelledJobs = nil
		report.RunningJobs = nil
		report.OtherChecks = nil
	}
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
// single job) and drills the failed and cancelled ones for their logs. Run
// targets bypass the PR-keyed cache, so logs are always re-downloaded.
func (a *app) runReport(ctx context.Context, tgt target.Target) (*model.Report, error) {
	info, failed, cancelled, running, err := a.client.RunReport(ctx, tgt.Owner, tgt.Repo, tgt.RunID, tgt.JobID)
	if err != nil {
		return nil, err
	}
	for i := range failed {
		a.drill(ctx, tgt.Owner, tgt.Repo, &failed[i])
	}
	for i := range cancelled {
		a.drill(ctx, tgt.Owner, tgt.Repo, &cancelled[i])
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
	return append(append(flags, "--"), positional...)
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
//
//nolint:gocritic // declared next to its constructor and helpers below
type app struct {
	client             *gh.Client
	opts               logs.Options
	maxCommandLines    int
	reviewCommentLimit int
	ciOnly             bool
	reviewsOnly        bool
}

// drillJobs fills in each job's log detail: on the same head commit it re-parses
// the cached raw log under the current extraction flags; otherwise it downloads
// the log (caching it for next time, unless noCache).
func (a *app) drillJobs(ctx context.Context, owner, repo string, number int, jobs []model.JobResult, sameCommit, noCache bool) {
	for i := range jobs {
		if sameCommit {
			if raw, ok, _ := cache.LoadJobLog(owner, repo, number, jobs[i].ID, jobs[i].RunAttempt); ok {
				jobs[i].FailedSteps = a.buildFailedSteps(jobs[i], raw)
				jobs[i].Inspected = true
				continue
			}
		}
		raw := a.drill(ctx, owner, repo, &jobs[i])
		if raw != "" && !noCache {
			if err := cache.SaveJobLog(owner, repo, number, jobs[i].ID, jobs[i].RunAttempt, raw); err != nil {
				fmt.Fprintln(os.Stderr, "shuck: warning: could not cache job log:", err)
			}
		}
	}
}

// drill downloads a failed job's log, extracts its failed steps, and returns the
// whole raw log so the caller can cache it for local re-parsing. It returns ""
// when the log could not be downloaded (nothing worth caching).
func (a *app) drill(ctx context.Context, owner, repo string, job *model.JobResult) string {
	job.Inspected = true
	raw, err := a.client.JobLog(ctx, owner, repo, job.ID)
	if err != nil {
		// A cancelled job legitimately may have no downloadable log (cancelled
		// before the runner started, or force-cancelled), so degrade to the
		// bare listing rather than reporting an error step.
		if model.IsCancelledConclusion(job.Conclusion) {
			job.Inspected = false
			return ""
		}
		job.FailedSteps = []model.FailedStep{{
			Name:    "(logs unavailable)",
			Excerpt: fmt.Sprintf("could not download logs: %v", err),
		}}
		return ""
	}
	job.FailedSteps = a.buildFailedSteps(*job, raw)
	return raw
}

// buildFailedSteps pairs the API's failed (or interrupted) steps with the log's
// error-bearing sections (by order) to recover each step's command and error
// excerpt. For a cancelled job the interrupted step carries an
// "##[error]The operation was canceled." marker, so the same pairing applies.
func (a *app) buildFailedSteps(job model.JobResult, raw string) []model.FailedStep {
	sections := logs.Parse(raw)
	errSecs := logs.ErrorSections(sections)

	var failedSteps []model.StepOverview
	for _, s := range job.Steps {
		if model.IsDrillableConclusion(s.Conclusion) {
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

	n := max(len(errSecs), len(failedSteps))
	if model.IsCancelledConclusion(job.Conclusion) {
		// A cancelled job often marks every not-yet-run step "cancelled", but
		// only the step that was actually interrupted has an error section.
		// Cap at the sections found so the queued steps don't each emit a
		// noisy "(no matching error log section found)" entry.
		n = len(errSecs)
	}
	out := make([]model.FailedStep, 0, n)
	for i := range n {
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
