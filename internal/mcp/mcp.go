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
	"github.com/justanotherspy/shuck/internal/compliance"
	"github.com/justanotherspy/shuck/internal/image"
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

const inspectImagesDesc = `List GHCR container images for an owner, or resolve one image to its digest.

Given a bare owner (or owner/repo), shuck lists every container image published
under that owner on GitHub Container Registry (ghcr.io), each with its latest tag
and immutable manifest digest (sha256:…). Given a full image reference like
ghcr.io/OWNER/NAME or ghcr.io/OWNER/NAME:v1.2, shuck resolves just that image to
its newest matching tag and digest, plus a digest-pinned reference ready to use
(ghcr.io/OWNER/NAME@sha256:… # tag). For a multi-arch image the digest is the
image-index digest, which is the correct value to pin.

Pass image as an owner, owner/repo, a github.com URL, or a ghcr.io/owner/name[:tag]
reference. Listing every image uses the GitHub Packages API and needs a token
with the read:packages scope (GITHUB_TOKEN or GH_TOKEN). Resolving a single
public image works without a token via the anonymous registry API; private
images need a token. Results are cached for an hour; set refresh to re-fetch.`

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

const checkComplianceDesc = `Check a repository's settings against its .shuck/compliance.yaml.

The .shuck/compliance.yaml file declares a repo's intended settings (merge
options, features, security, branch protection). shuck reads the repo's live
settings via the GitHub API and returns, per declared setting, whether the repo
matches — pass, fail (drift), or skipped (the setting could not be read with the
current token). Reach for this to verify a repo is configured the way its policy
says, or to find what drifted.

Target selection: pass url for a github.com/<owner>/<repo>[/...] URL, or repo as
owner/repo, or nothing to use the repo of the local working directory. The config
is fetched from the repo by default (set ref to pick a branch/tag/SHA); set
config to read a local file path instead. Requires a GitHub token in GITHUB_TOKEN
or GH_TOKEN; reading branch protection and security settings needs the repo scope
and admin access.`

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
		Name:        "check_compliance",
		Title:       "Check a repo's settings against its compliance config",
		Description: checkComplianceDesc,
		Annotations: annotations,
	}, checkCompliance)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "inspect_action",
		Title:       "Resolve an Action to a SHA pin",
		Description: inspectActionDesc,
		Annotations: annotations,
	}, inspectAction)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "inspect_images",
		Title:       "List GHCR images or resolve one to a digest",
		Description: inspectImagesDesc,
		Annotations: annotations,
	}, inspectImages)

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

type checkComplianceInput struct {
	Repo   string `json:"repo,omitempty" jsonschema:"GitHub repository as owner/repo. If omitted, it is inferred from the local working directory's origin remote."`
	URL    string `json:"url,omitempty" jsonschema:"A github.com/<owner>/<repo>[/...] URL. Takes precedence over repo."`
	Config string `json:"config,omitempty" jsonschema:"Path to a local compliance config file. If omitted, .shuck/compliance.yaml is fetched from the repo."`
	Ref    string `json:"ref,omitempty" jsonschema:"Git ref (branch/tag/SHA) to fetch the config from when reading it from the repo. Default: the repo's default branch."`
}

func checkCompliance(ctx context.Context, _ *mcp.CallToolRequest, in checkComplianceInput) (*mcp.CallToolResult, compliance.Document, error) {
	owner, repo, err := target.ResolveRepo(in.targetArgs())
	if err != nil {
		return nil, compliance.Document{}, err
	}
	report, err := cli.Compliance(ctx, owner, repo, cli.ComplianceOptions{
		ConfigPath:  in.Config,
		Ref:         in.Ref,
		PreferLocal: in.Config == "" && in.Repo == "" && in.URL == "",
	})
	if err != nil {
		return nil, compliance.Document{}, err
	}
	var b strings.Builder
	compliance.Render(&b, report)
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}
	return res, compliance.NewDocument(report), nil
}

// targetArgs maps the tool inputs onto the positional args target.ResolveRepo
// understands, so the MCP and CLI share one resolution path.
func (in checkComplianceInput) targetArgs() []string {
	switch {
	case in.URL != "":
		return []string{in.URL}
	case in.Repo != "":
		return []string{in.Repo}
	default:
		return nil // local repo
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

type inspectImagesInput struct {
	Image   string `json:"image,omitempty" jsonschema:"An owner, owner/repo, a github.com URL, or a ghcr.io/owner/name[:tag] reference. An owner (or repo) lists every image under it; a full ghcr.io reference resolves that one image. If omitted, the local working directory's owner is used."`
	Refresh bool   `json:"refresh,omitempty" jsonschema:"Ignore the cache and re-fetch (results are cached for an hour)."`
}

// imagesOutput is the structured result of inspect_images: exactly one of
// Resolved (a single image pinned to its digest) or List (every image under an
// owner) is populated, mirroring the CLI's two modes.
type imagesOutput struct {
	Resolved *image.Document     `json:"resolved,omitempty"`
	List     *image.ListDocument `json:"list,omitempty"`
}

func inspectImages(ctx context.Context, _ *mcp.CallToolRequest, in inspectImagesInput) (*mcp.CallToolResult, imagesOutput, error) {
	// A full ghcr.io/owner/name reference resolves a single image; anything else
	// (an owner, owner/repo, URL, or nothing) lists every image under the owner —
	// the same split the CLI's `shuck image` makes.
	if isImageRef(in.Image) {
		ref, err := image.ParseRef(in.Image)
		if err != nil {
			return nil, imagesOutput{}, err
		}
		if ref.ListAll() {
			return nil, imagesOutput{}, fmt.Errorf("an image reference needs a name (ghcr.io/owner/name)")
		}
		resolved, err := cli.Image(ctx, ref, cli.ImageOptions{Refresh: in.Refresh})
		if err != nil {
			return nil, imagesOutput{}, err
		}
		var b strings.Builder
		image.Render(&b, resolved)
		doc := image.NewDocument(resolved)
		res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}
		return res, imagesOutput{Resolved: &doc}, nil
	}

	owner, err := imageOwner(in.Image)
	if err != nil {
		return nil, imagesOutput{}, err
	}
	pkgs, err := cli.Images(ctx, owner, cli.ImageOptions{Refresh: in.Refresh})
	if err != nil {
		return nil, imagesOutput{}, err
	}
	var b strings.Builder
	image.RenderList(&b, image.DefaultRegistry, owner, pkgs)
	doc := image.NewListDocument(image.DefaultRegistry, owner, pkgs)
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}
	return res, imagesOutput{List: &doc}, nil
}

// isImageRef reports whether s is an explicit ghcr.io/... reference (single-image
// resolution) rather than an owner / repo / URL (listing).
func isImageRef(s string) bool {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	first, _, ok := strings.Cut(s, "/")
	return ok && strings.EqualFold(first, image.DefaultRegistry)
}

// imageOwner resolves the owner to list images for from the tool input: a bare
// token is the owner directly; an owner/repo or github.com URL yields the repo's
// owner; an empty string uses the local repo. It mirrors the CLI's resolution.
func imageOwner(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s != "" && !strings.Contains(s, "/") && !strings.Contains(s, "://") {
		return s, nil
	}
	var args []string
	if s != "" {
		args = []string{s}
	}
	owner, _, err := target.ResolveRepo(args)
	return owner, err
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
