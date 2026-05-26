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

	"github.com/justanotherspy/shuck/internal/action"
	"github.com/justanotherspy/shuck/internal/cli"
	"github.com/justanotherspy/shuck/internal/jsonout"
	"github.com/justanotherspy/shuck/internal/logs"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/render"
	"github.com/justanotherspy/shuck/internal/security"
	"github.com/justanotherspy/shuck/internal/target"
)

const serverName = "shuck"

const inspectLogsDesc = `Show the exact failing CI step logs for a GitHub pull request, or a single run.

shuck resolves the PR, reads its checks via the GitHub API, drills the failed
GitHub Actions jobs down to the failing steps, and returns each failed step's
command plus the extracted error excerpt from its log. Non-Actions checks are
listed by name (no logs exist for them). This is the first move when CI goes
red on a PR. (For the PR's reviews, use inspect_reviews.)

Target selection (most specific wins): pass url for a PR URL; or repo + pr for
an explicit PR; or pr alone to use the repo of the local working directory; or
nothing to inspect the open PR for the current branch. To inspect one workflow
run instead of a PR, set run to a run/job URL, or to a bare run ID together
with repo — run targets bypass shuck's cache, so logs are always freshly
downloaded. Requires a GitHub token in GITHUB_TOKEN or GH_TOKEN in the server's
environment.`

const inspectReviewsDesc = `Summarize a GitHub pull request's reviews and review-comment threads.

shuck returns the PR's reviews (approve / changes requested / commented) and
their inline comment threads, collapsing resolved or outdated threads to just
the reason and capping comments per active thread. Use this to see what
reviewers asked for without paging through the PR UI. (For failing CI logs, use
inspect_logs.)

Target selection (most specific wins): pass url for a PR URL; or repo + pr for
an explicit PR; or pr alone to use the repo of the local working directory; or
nothing to inspect the open PR for the current branch. Requires a GitHub token
in GITHUB_TOKEN or GH_TOKEN in the server's environment.`

const inspectActionDesc = `Resolve a GitHub Action to its latest tag and commit SHA, for SHA-pinning.

Given an action reference like actions/checkout or actions/checkout@v4, shuck
finds the latest tag matching the requested major / major.minor (preferring a
stable release over a prerelease) and returns the tag, the commit SHA, and a
pin line ready to drop after "uses:" (owner/action@<sha> # <tag>). Reach for
this to pin a workflow's actions to immutable SHAs.

Pass action as owner/action[/subpath][@version]. Auth is optional for public
repos; a GitHub token in GITHUB_TOKEN or GH_TOKEN lifts the unauthenticated
rate limit. Tags are cached for a day; set refresh to re-fetch.`

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
		Name:        "inspect_logs",
		Title:       "Inspect a PR's (or run's) failing CI",
		Description: inspectLogsDesc,
		Annotations: annotations,
	}, inspectLogs)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "inspect_reviews",
		Title:       "Inspect a PR's reviews",
		Description: inspectReviewsDesc,
		Annotations: annotations,
	}, inspectReviews)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "inspect_security",
		Title:       "Inspect a repo's security alerts",
		Description: inspectSecurityDesc,
		Annotations: annotations,
	}, inspectSecurity)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "inspect_action",
		Title:       "Resolve an Action to a SHA pin",
		Description: inspectActionDesc,
		Annotations: annotations,
	}, inspectAction)

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

type inspectLogsInput struct {
	Repo string `json:"repo,omitempty" jsonschema:"GitHub repository as owner/repo. If omitted, it is inferred from the local working directory's origin remote."`
	PR   int    `json:"pr,omitempty" jsonschema:"Pull request number. If omitted, shuck finds the open PR for the current branch (requires a local repo)."`
	URL  string `json:"url,omitempty" jsonschema:"A GitHub pull request URL such as https://github.com/owner/repo/pull/42. Takes precedence over repo and pr."`
	Run  string `json:"run,omitempty" jsonschema:"Inspect a single workflow run instead of a PR: a run/job URL (.../actions/runs/123 or .../job/456), or a bare run ID together with repo. Takes precedence over url/repo/pr."`

	extractInput

	Refresh bool `json:"refresh,omitempty" jsonschema:"Ignore and rebuild the cache (use when CI was re-run and cached results look stale)."`
	NoCache bool `json:"no_cache,omitempty" jsonschema:"Do not read or write the cache."`
	Offline bool `json:"offline,omitempty" jsonschema:"Render only from the local cache, without network access. Requires repo and pr."`
}

func inspectLogs(ctx context.Context, _ *mcp.CallToolRequest, in inspectLogsInput) (*mcp.CallToolResult, jsonout.Document, error) {
	tgt, err := in.target()
	if err != nil {
		return nil, jsonout.Document{}, err
	}

	opts := in.apply(defaultOptions())
	opts.CIOnly = true
	opts.Refresh = in.Refresh
	opts.NoCache = in.NoCache
	opts.Offline = in.Offline

	report, err := cli.Inspect(ctx, tgt, opts)
	if err != nil {
		return nil, jsonout.Document{}, err
	}
	return toResult(report)
}

// target resolves the tool inputs to a PR target, or — when run is set — a
// run/job target, sharing the CLI's resolution path.
func (in inspectLogsInput) target() (target.Target, error) {
	if in.Run != "" {
		if strings.ContainsAny(in.Run, "/:") {
			tgt, err := target.Resolve([]string{in.Run})
			if err != nil {
				return target.Target{}, err
			}
			if tgt.RunID == 0 {
				return target.Target{}, fmt.Errorf("run %q is not a GitHub Actions run or job URL", in.Run)
			}
			return tgt, nil
		}
		id, err := strconv.ParseInt(in.Run, 10, 64)
		if err != nil || id <= 0 {
			return target.Target{}, fmt.Errorf("run must be a run ID or a run/job URL, got %q", in.Run)
		}
		owner, repo, ok := strings.Cut(in.Repo, "/")
		if !ok || owner == "" || repo == "" {
			return target.Target{}, fmt.Errorf("repo (owner/repo) is required when run is a bare run ID")
		}
		return target.Target{Owner: owner, Repo: repo, RunID: id}, nil
	}
	args, err := prTargetArgs(in.URL, in.Repo, in.PR)
	if err != nil {
		return target.Target{}, err
	}
	return target.Resolve(args)
}

type inspectReviewsInput struct {
	Repo string `json:"repo,omitempty" jsonschema:"GitHub repository as owner/repo. If omitted, it is inferred from the local working directory's origin remote."`
	PR   int    `json:"pr,omitempty" jsonschema:"Pull request number. If omitted, shuck finds the open PR for the current branch (requires a local repo)."`
	URL  string `json:"url,omitempty" jsonschema:"A GitHub pull request URL such as https://github.com/owner/repo/pull/42. Takes precedence over repo and pr."`

	ReviewCommentLimit *int `json:"review_comment_limit,omitempty" jsonschema:"Max comments shown per active review thread (default 5)."`

	Refresh bool `json:"refresh,omitempty" jsonschema:"Ignore and rebuild the cache."`
	NoCache bool `json:"no_cache,omitempty" jsonschema:"Do not read or write the cache."`
	Offline bool `json:"offline,omitempty" jsonschema:"Render only from the local cache, without network access. Requires repo and pr."`
}

func inspectReviews(ctx context.Context, _ *mcp.CallToolRequest, in inspectReviewsInput) (*mcp.CallToolResult, jsonout.Document, error) {
	args, err := prTargetArgs(in.URL, in.Repo, in.PR)
	if err != nil {
		return nil, jsonout.Document{}, err
	}
	tgt, err := target.Resolve(args)
	if err != nil {
		return nil, jsonout.Document{}, err
	}
	if tgt.RunID != 0 {
		return nil, jsonout.Document{}, fmt.Errorf("reviews require a PR target, not a run/job URL")
	}

	opts := defaultOptions()
	if in.ReviewCommentLimit != nil {
		opts.ReviewCommentLimit = *in.ReviewCommentLimit
	}
	opts.ReviewsOnly = true
	opts.Refresh = in.Refresh
	opts.NoCache = in.NoCache
	opts.Offline = in.Offline

	report, err := cli.Inspect(ctx, tgt, opts)
	if err != nil {
		return nil, jsonout.Document{}, err
	}
	return toResult(report)
}

// prTargetArgs maps PR tool inputs onto the positional arguments that
// target.Resolve understands, so the MCP and CLI share one resolution path. A
// nil result means "the local repo's current-branch PR".
func prTargetArgs(url, repo string, pr int) ([]string, error) {
	switch {
	case url != "":
		return []string{url}, nil
	case repo != "" && pr > 0:
		return []string{repo, strconv.Itoa(pr)}, nil
	case repo != "":
		return nil, fmt.Errorf("pr is required when repo is set")
	case pr > 0:
		return []string{strconv.Itoa(pr)}, nil
	default:
		return nil, nil
	}
}

type inspectActionInput struct {
	Action  string `json:"action" jsonschema:"GitHub Action as owner/action[/subpath][@version], e.g. actions/checkout@v4. Omit the version for the latest stable release."`
	Refresh bool   `json:"refresh,omitempty" jsonschema:"Ignore the cached tag list and re-fetch (tags are cached for a day)."`
}

func inspectAction(ctx context.Context, _ *mcp.CallToolRequest, in inspectActionInput) (*mcp.CallToolResult, action.Document, error) {
	ref, err := action.ParseRef(in.Action)
	if err != nil {
		return nil, action.Document{}, err
	}
	resolved, err := cli.Action(ctx, ref, cli.ActionOptions{Refresh: in.Refresh})
	if err != nil {
		return nil, action.Document{}, err
	}
	var b strings.Builder
	action.Render(&b, resolved)
	res := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: b.String()}},
	}
	return res, action.NewDocument(resolved), nil
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
