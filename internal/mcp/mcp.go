// Package mcp exposes shuck's CI-failure inspection over a local (stdio) Model
// Context Protocol server, so MCP-aware agents can pull the exact failing CI
// step logs as typed tool calls instead of scraping the CLI's text output.
//
// It is a thin front-end: each tool resolves a target and delegates to
// cli.Inspect (the same pipeline the CLI uses), then returns the rendered
// human-readable report as text content alongside the stable jsonout.Document
// as typed structured output.
package mcp

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/justanotherspy/shuck/internal/cli"
	"github.com/justanotherspy/shuck/internal/jsonout"
	"github.com/justanotherspy/shuck/internal/logs"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/render"
	"github.com/justanotherspy/shuck/internal/security"
	"github.com/justanotherspy/shuck/internal/target"
)

const serverName = "shuck"

const inspectPRDesc = `Show the exact failing CI step logs for a GitHub pull request.

shuck resolves the PR, reads its checks via the GitHub API, drills the failed
GitHub Actions jobs down to the failing steps, and returns each failed step's
command plus the extracted error excerpt from its log. Non-Actions checks are
listed by name (no logs exist for them). It also summarizes the PR's reviews
(approve / changes requested / commented) and their inline comment threads,
collapsing resolved or outdated threads to just the reason. This is the first
move when CI goes red on a PR.

Target selection (most specific wins): pass url for a PR URL; or repo + pr for
an explicit PR; or pr alone to use the repo of the local working directory; or
nothing to inspect the open PR for the current branch. Use ci_only or
reviews_only to focus on one dimension. Requires a GitHub token in GITHUB_TOKEN
or GH_TOKEN in the server's environment.`

const inspectRunDesc = `Inspect a single GitHub Actions workflow run, or one job within it.

Use this when a CI-failure notification points directly at a run or job: it
skips the PR-wide scan and drills just that target's failed steps for their
error logs. Run targets bypass shuck's cache, so logs are always freshly
downloaded.

Pass url for an Actions run or job URL, or repo + run_id (optionally with
job_id to restrict to one job). Requires a GitHub token in GITHUB_TOKEN or
GH_TOKEN in the server's environment.`

const inspectSecurityDesc = `Summarize a repository's GitHub security alerts in one shot.

shuck pulls a repo's code scanning, secret scanning, and Dependabot alerts and
returns one summary: severity, file:line, the vulnerable package and its fix
version, and CVE/GHSA IDs (npm malware advisories surface as Dependabot alerts).
Raw secret values are never returned. A source that is not enabled, or not
visible to the token, is reported and skipped rather than failing — so a repo
with only some features enabled still produces output. Reach for this to triage
a repo's security posture or decide what to fix first.

Target selection: pass url for a github.com/<owner>/<repo>[/...] URL, or repo as
owner/repo, or nothing to use the repo of the local working directory. state
filters which alerts to show (open by default; open|all|dismissed|fixed|resolved).
Requires a GitHub token in GITHUB_TOKEN or GH_TOKEN in the server's environment;
private-repo security data needs the security_events (or repo) scope.`

// Serve runs the shuck MCP server over stdio until the client disconnects or
// ctx is cancelled. It takes no positional arguments.
func Serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("shuck mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "shuck mcp — run a local MCP (stdio) server exposing shuck's failing-CI-log tools.")
		fmt.Fprintln(os.Stderr, "It speaks the Model Context Protocol on stdin/stdout and takes no arguments.")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("shuck mcp takes no arguments, got %q", fs.Arg(0))
	}

	server := newServer()
	return server.Run(ctx, &mcp.StdioTransport{})
}

// newServer builds the MCP server with shuck's tools registered.
func newServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: cli.Version()}, nil)

	open := true
	annotations := &mcp.ToolAnnotations{
		ReadOnlyHint:   true,
		IdempotentHint: true,
		OpenWorldHint:  &open,
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "inspect_pr",
		Title:       "Inspect a PR's failing CI",
		Description: inspectPRDesc,
		Annotations: annotations,
	}, inspectPR)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "inspect_run",
		Title:       "Inspect a workflow run or job",
		Description: inspectRunDesc,
		Annotations: annotations,
	}, inspectRun)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "inspect_security",
		Title:       "Inspect a repo's security alerts",
		Description: inspectSecurityDesc,
		Annotations: annotations,
	}, inspectSecurity)

	return s
}

// extractInput is the subset of options that tune log extraction, shared by
// both tools. The three sizing knobs are pointers so an omitted value falls
// back to shuck's documented default rather than zero.
type extractInput struct {
	Context         *int   `json:"context,omitempty" jsonschema:"Lines of context kept around each error match (default 10)."`
	ShortThreshold  *int   `json:"short_threshold,omitempty" jsonschema:"Logs with at most this many lines are shown whole (default 100)."`
	Tail            *int   `json:"tail,omitempty" jsonschema:"Lines tailed when a long log has no error match (default 100)."`
	Pattern         string `json:"pattern,omitempty" jsonschema:"Override the error-matching regular expression."`
	Full            bool   `json:"full,omitempty" jsonschema:"Show full, untrimmed logs for failed steps instead of trimmed excerpts."`
	MaxCommandLines *int   `json:"max_command_lines,omitempty" jsonschema:"Max lines of a failed step's command to show; longer commands are truncated (default 30, 0 = no limit)."`
}

// apply layers the request's extraction knobs onto a base options value,
// keeping shuck's defaults for any knob the caller left unset.
func (e extractInput) apply(o cli.InspectOptions) cli.InspectOptions {
	if e.Context != nil {
		o.Context = *e.Context
	}
	if e.ShortThreshold != nil {
		o.ShortThreshold = *e.ShortThreshold
	}
	if e.Tail != nil {
		o.Tail = *e.Tail
	}
	if e.MaxCommandLines != nil {
		o.MaxCommandLines = *e.MaxCommandLines
	}
	o.Pattern = e.Pattern
	o.Full = e.Full
	return o
}

// defaultOptions returns InspectOptions seeded with shuck's documented
// extraction defaults, sourced from logs.DefaultOptions so the two never drift.
func defaultOptions() cli.InspectOptions {
	d := logs.DefaultOptions()
	return cli.InspectOptions{
		Context:            d.Context,
		ShortThreshold:     d.ShortThreshold,
		Tail:               d.Tail,
		MaxCommandLines:    logs.DefaultMaxCommandLines,
		ReviewCommentLimit: 5,
	}
}

type inspectPRInput struct {
	Repo string `json:"repo,omitempty" jsonschema:"GitHub repository as owner/repo. If omitted, it is inferred from the local working directory's origin remote."`
	PR   int    `json:"pr,omitempty" jsonschema:"Pull request number. If omitted, shuck finds the open PR for the current branch (requires a local repo)."`
	URL  string `json:"url,omitempty" jsonschema:"A GitHub pull request URL such as https://github.com/owner/repo/pull/42. Takes precedence over repo and pr."`

	extractInput

	ReviewCommentLimit *int `json:"review_comment_limit,omitempty" jsonschema:"Max comments shown per active review thread (default 5)."`
	CIOnly             bool `json:"ci_only,omitempty" jsonschema:"Show only CI checks; skip PR reviews."`
	ReviewsOnly        bool `json:"reviews_only,omitempty" jsonschema:"Show only PR reviews; skip CI checks."`

	Refresh bool `json:"refresh,omitempty" jsonschema:"Ignore and rebuild the cache (use when CI was re-run and cached results look stale)."`
	NoCache bool `json:"no_cache,omitempty" jsonschema:"Do not read or write the cache."`
	Offline bool `json:"offline,omitempty" jsonschema:"Render only from the local cache, without network access. Requires repo and pr."`
}

func inspectPR(ctx context.Context, _ *mcp.CallToolRequest, in inspectPRInput) (*mcp.CallToolResult, jsonout.Document, error) {
	args, err := in.targetArgs()
	if err != nil {
		return nil, jsonout.Document{}, err
	}
	tgt, err := target.Resolve(args)
	if err != nil {
		return nil, jsonout.Document{}, err
	}

	opts := in.apply(defaultOptions())
	if in.ReviewCommentLimit != nil {
		opts.ReviewCommentLimit = *in.ReviewCommentLimit
	}
	opts.CIOnly = in.CIOnly
	opts.ReviewsOnly = in.ReviewsOnly
	opts.Refresh = in.Refresh
	opts.NoCache = in.NoCache
	opts.Offline = in.Offline

	report, err := cli.Inspect(ctx, tgt, opts)
	if err != nil {
		return nil, jsonout.Document{}, err
	}
	return toResult(report)
}

// targetArgs maps the tool inputs onto the positional arguments that
// target.Resolve understands, so the MCP and CLI share one resolution path.
func (in inspectPRInput) targetArgs() ([]string, error) {
	switch {
	case in.URL != "":
		return []string{in.URL}, nil
	case in.Repo != "" && in.PR > 0:
		return []string{in.Repo, strconv.Itoa(in.PR)}, nil
	case in.Repo != "":
		return nil, fmt.Errorf("pr is required when repo is set")
	case in.PR > 0:
		return []string{strconv.Itoa(in.PR)}, nil
	default:
		return nil, nil // local repo + current branch
	}
}

type inspectRunInput struct {
	URL   string `json:"url,omitempty" jsonschema:"A GitHub Actions run or job URL, e.g. .../actions/runs/123 or .../actions/runs/123/job/456. Takes precedence over repo, run_id, and job_id."`
	Repo  string `json:"repo,omitempty" jsonschema:"GitHub repository as owner/repo. Required if url is not given."`
	RunID int64  `json:"run_id,omitempty" jsonschema:"Workflow run ID. Required if url is not given."`
	JobID int64  `json:"job_id,omitempty" jsonschema:"Restrict the inspection to a single job ID within the run. Optional."`

	extractInput
}

func inspectRun(ctx context.Context, _ *mcp.CallToolRequest, in inspectRunInput) (*mcp.CallToolResult, jsonout.Document, error) {
	tgt, err := in.target()
	if err != nil {
		return nil, jsonout.Document{}, err
	}
	report, err := cli.Inspect(ctx, tgt, in.apply(defaultOptions()))
	if err != nil {
		return nil, jsonout.Document{}, err
	}
	return toResult(report)
}

// target builds a run/job target from a URL or from explicit repo + run_id.
func (in inspectRunInput) target() (target.Target, error) {
	if in.URL != "" {
		tgt, err := target.Resolve([]string{in.URL})
		if err != nil {
			return target.Target{}, err
		}
		if tgt.RunID == 0 {
			return target.Target{}, fmt.Errorf("url %q is not a GitHub Actions run or job URL", in.URL)
		}
		return tgt, nil
	}
	if in.Repo != "" && in.RunID > 0 {
		owner, repo, ok := strings.Cut(in.Repo, "/")
		if !ok || owner == "" || repo == "" {
			return target.Target{}, fmt.Errorf("invalid repo %q (expected owner/repo)", in.Repo)
		}
		return target.Target{Owner: owner, Repo: repo, RunID: in.RunID, JobID: in.JobID}, nil
	}
	return target.Target{}, fmt.Errorf("provide either url, or repo and run_id")
}

// toResult packages a report as a tool response: the rendered, human-readable
// report as text content and the stable JSON document as typed structured
// output. The SDK populates StructuredContent from the returned document.
func toResult(report *model.Report) (*mcp.CallToolResult, jsonout.Document, error) {
	var b strings.Builder
	render.Report(&b, report)
	res := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: b.String()}},
	}
	return res, jsonout.NewDocument(report), nil
}

type inspectSecurityInput struct {
	Repo    string `json:"repo,omitempty" jsonschema:"GitHub repository as owner/repo. If omitted, it is inferred from the local working directory's origin remote."`
	URL     string `json:"url,omitempty" jsonschema:"A github.com/<owner>/<repo>[/...] URL (a plain repo, PR, or Actions URL). Takes precedence over repo."`
	State   string `json:"state,omitempty" jsonschema:"Alert state to show: open (default), all, dismissed, fixed, or resolved."`
	Refresh bool   `json:"refresh,omitempty" jsonschema:"Ignore and rebuild the cache (alerts are cached for an hour)."`
}

func inspectSecurity(ctx context.Context, _ *mcp.CallToolRequest, in inspectSecurityInput) (*mcp.CallToolResult, security.Document, error) {
	owner, repo, err := target.ResolveRepo(in.targetArgs())
	if err != nil {
		return nil, security.Document{}, err
	}
	report, err := cli.Security(ctx, owner, repo, cli.SecurityOptions{State: in.State, Refresh: in.Refresh})
	if err != nil {
		return nil, security.Document{}, err
	}
	return toSecurityResult(report)
}

// targetArgs maps the tool inputs onto the positional args target.ResolveRepo
// understands, so the MCP and CLI share one resolution path.
func (in inspectSecurityInput) targetArgs() []string {
	switch {
	case in.URL != "":
		return []string{in.URL}
	case in.Repo != "":
		return []string{in.Repo}
	default:
		return nil // local repo
	}
}

func toSecurityResult(report *model.SecurityReport) (*mcp.CallToolResult, security.Document, error) {
	var b strings.Builder
	security.Render(&b, report)
	res := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: b.String()}},
	}
	return res, security.NewDocument(report), nil
}
