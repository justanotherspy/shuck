package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/justanotherspy/shuck/internal/cli"
	"github.com/justanotherspy/shuck/internal/monitor"
	"github.com/justanotherspy/shuck/internal/pins"
)

const monitorStatusDesc = `Ask the shuck background monitor what it is watching and where those pull requests stand.

The monitor is a local daemon that follows the working trees you are actually
working in: it reads the branch out of HEAD, finds the open PR for it, and
re-checks on a cadence that tightens while CI is running. It retargets itself
when you switch branches or worktrees, so it is never pointed at the wrong PR.

This returns, per watched working tree, which PR it resolves to; per PR, the
last CI verdict, the head commit, the PR's lifecycle, and when it will next be
checked; plus the GitHub quota headroom and how many events are waiting for
you. Reach for it to answer "is my PR green?" without spending a fetch, or to
find out why the monitor has gone quiet.

If no monitor is running this starts one. Pass consumer (your session
identifier) to also learn how many events are waiting specifically for you.`

const monitorEventsDesc = `Collect what the shuck background monitor has noticed since you last looked.

Each event carries a one-line headline and an agent-ready body: a failed job
comes with its distilled failing-step logs, a review comment with its diff hunk
and the surrounding lines of the file, a stale action pin with the corrected
"uses:" line to paste. Events are delivered once per consumer, so two calls do
not repeat the same news.

Kinds: ci.failed, ci.passed, ci.started, review.comment, review.submitted,
pr.state, pins.stale, watch.target, monitor.error.

Set wait_seconds to block until something happens — that is how to wait for CI
to finish after a push without polling: watch the PR, push, then call this with
wait_seconds and act on the ci.passed or ci.failed that comes back. Set peek to
look without consuming, and all to re-read the whole retained journal.

In a Claude Code session with the shuck plugin installed you do not normally
need this: events are delivered into the conversation as they arrive. Use it
when you want to wait for one, or to re-read something.`

const monitorWatchDesc = `Tell the shuck background monitor to start following something.

With no arguments it follows the current working directory: from then on the
monitor tracks whichever pull request that tree's branch belongs to, and
retargets itself when the branch changes. That is the normal case and it is
what a Claude Code session does automatically on startup.

Pass path to follow a different working tree, or repo + pr (or url) to pin one
specific pull request — useful for keeping an eye on someone else's PR, or on
one you are not checked out on. Watching something already watched is
harmless; it just refreshes it.

Watches expire after half a day without anyone asking about them, and the
monitor exits once it has nothing left to watch. Use monitor_unwatch to stop
following something sooner.`

const monitorUnwatchDesc = `Tell the shuck background monitor to stop following something.

Pass the same target you watched: nothing for the current working directory,
path for another tree, or repo + pr / url for a pinned pull request. You can
also pass the watch's id straight from monitor_status.`

const checkPinsDesc = `Find the GitHub Actions in a checkout's workflows that are not pinned to a commit SHA, or whose pin has gone stale.

"uses: actions/checkout@v4" runs whatever commit that tag points at today —
the tag can be moved. Pinning to a SHA fixes it, but a pin left alone falls
behind the releases it was taken from, so this reports both: references still
on a mutable tag, and pins whose "# v4.2.2" comment names a release that has
since been superseded. Every finding comes with the exact corrected line.

Reach for this right after writing or editing a workflow file, and before
opening a PR that touches one. It scans .github/workflows plus any composite
action.yml, so it covers reusable workflows and local composite actions too.

Auth is optional for public actions; a token in GITHUB_TOKEN or GH_TOKEN lifts
the unauthenticated rate limit. Tag lists are cached for an hour. Set offline
to list the references without resolving their latest releases.`

// registerMonitorTools adds the background monitor's tools plus the pin audit
// to the server. They are split out from the inspection tools because they
// answer a different question: not "what does this PR look like right now" but
// "tell me when it changes".
func registerMonitorTools(s *mcp.Server, readOnly *mcp.ToolAnnotations) {
	open := true
	mutating := &mcp.ToolAnnotations{OpenWorldHint: &open, IdempotentHint: true}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "monitor_status",
		Title:       "What the background monitor is watching",
		Description: monitorStatusDesc,
		Annotations: readOnly,
	}, monitorStatusTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "monitor_events",
		Title:       "Collect what the background monitor noticed",
		Description: monitorEventsDesc,
		Annotations: mutating,
	}, monitorEventsTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "monitor_watch",
		Title:       "Follow a working tree or PR in the background",
		Description: monitorWatchDesc,
		Annotations: mutating,
	}, monitorWatchTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "monitor_unwatch",
		Title:       "Stop following something",
		Description: monitorUnwatchDesc,
		Annotations: mutating,
	}, monitorUnwatchTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "check_pins",
		Title:       "Audit workflow action pins",
		Description: checkPinsDesc,
		Annotations: readOnly,
	}, checkPins)
}

// newMonitorClient builds the client the monitor tools talk through. It is a
// package var so tests can stand in a fake daemon.
var newMonitorClient = monitor.NewClient

type monitorStatusInput struct {
	Consumer string `json:"consumer,omitempty" jsonschema:"Your session identifier. Supplying it reports how many events are waiting specifically for you; events are delivered once per consumer."`
}

func monitorStatusTool(ctx context.Context, _ *mcp.CallToolRequest, in monitorStatusInput) (*mcp.CallToolResult, monitor.Status, error) {
	client, err := newMonitorClient()
	if err != nil {
		return nil, monitor.Status{}, err
	}
	st, err := client.Status(ctx, in.Consumer)
	if err != nil {
		return nil, monitor.Status{}, err
	}
	return textResult(renderMonitorStatus(st)), *st, nil
}

// renderMonitorStatus words the status for a reader rather than a parser. The
// structured half of the response carries the same data; this is the half that
// gets read.
func renderMonitorStatus(st *monitor.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "shuck monitor %s is running (up %s).\n", st.Version, st.Uptime)
	if len(st.Watches) == 0 {
		b.WriteString("It is not watching anything — call monitor_watch to point it at a working tree.\n")
	}
	for _, w := range st.Watches {
		fmt.Fprintf(&b, "  watching %s\n", w.Describe())
	}
	for _, t := range st.Targets {
		verdict := "checks running or not yet started"
		switch t.Verdict {
		case "passed":
			verdict = "all checks passed"
		case "failed":
			verdict = "CHECKS FAILING"
		}
		fmt.Fprintf(&b, "  %s: %s", t.Target, verdict)
		if t.Lifecycle != "" && t.Lifecycle != "open" {
			fmt.Fprintf(&b, " (%s)", t.Lifecycle)
		}
		if t.LastError != "" {
			fmt.Fprintf(&b, " — last check failed: %s", t.LastError)
		}
		b.WriteString("\n")
	}
	if st.RateLimit > 0 {
		fmt.Fprintf(&b, "GitHub quota: %d/%d remaining.\n", st.RateRemaining, st.RateLimit)
	}
	fmt.Fprintf(&b, "%d event(s) recorded", st.Events)
	if st.Pending > 0 {
		fmt.Fprintf(&b, "; %d waiting for you — call monitor_events.", st.Pending)
	}
	return b.String()
}

type monitorEventsInput struct {
	Consumer string `json:"consumer,omitempty" jsonschema:"Your session identifier, so events are delivered to you exactly once. Omit to peek without consuming anyone's backlog."`
	Limit    int    `json:"limit,omitempty" jsonschema:"At most this many events (0 = no limit). When more are pending the newest are returned."`
	Wait     int    `json:"wait_seconds,omitempty" jsonschema:"Block for up to this many seconds when nothing is pending. Use it to wait for CI to finish instead of polling."`
	Peek     bool   `json:"peek,omitempty" jsonschema:"Return the pending events without consuming them, so they are delivered again later."`
	All      bool   `json:"all,omitempty" jsonschema:"Return the whole retained journal instead of only what is new."`
}

// eventsDocument is the structured half of monitor_events.
type eventsDocument struct {
	Events []monitor.Event `json:"events"`
	Cursor uint64          `json:"cursor"`
}

func monitorEventsTool(ctx context.Context, _ *mcp.CallToolRequest, in monitorEventsInput) (*mcp.CallToolResult, eventsDocument, error) {
	client, err := newMonitorClient()
	if err != nil {
		return nil, eventsDocument{}, err
	}
	// A tool call is not the moment to start a daemon: if none is running
	// there is nothing it could have noticed yet, and starting one here would
	// return an empty answer that looks like "all clear".
	client.AutoStart = false

	req := monitor.Request{
		Consumer: in.Consumer,
		Limit:    in.Limit,
		Peek:     in.Peek,
		All:      in.All,
	}
	if in.Wait > 0 {
		req.Wait = time.Duration(in.Wait) * time.Second
		// Give the round trip room beyond the daemon's own wait so the
		// connection is not cut a moment before the answer arrives.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Wait+30*time.Second)
		defer cancel()
	}

	events, cursor, err := client.Events(ctx, req)
	if err != nil {
		return nil, eventsDocument{}, err
	}
	text := monitor.FormatFeed(events)
	if text == "" {
		text = "Nothing new from the monitor."
	}
	return textResult(text), eventsDocument{Events: events, Cursor: cursor}, nil
}

type monitorWatchInput struct {
	Path string `json:"path,omitempty" jsonschema:"A working tree to follow. Defaults to the server's working directory. The monitor tracks whichever PR that tree's branch belongs to and retargets itself when the branch changes."`
	Repo string `json:"repo,omitempty" jsonschema:"Pin one pull request instead of following a tree: the repository as owner/repo, with pr."`
	PR   int    `json:"pr,omitempty" jsonschema:"Pull request number, with repo."`
	URL  string `json:"url,omitempty" jsonschema:"A GitHub pull request URL to pin. Takes precedence over repo and pr."`
}

func monitorWatchTool(ctx context.Context, _ *mcp.CallToolRequest, in monitorWatchInput) (*mcp.CallToolResult, monitor.Watch, error) {
	spec, err := in.spec()
	if err != nil {
		return nil, monitor.Watch{}, err
	}
	client, err := newMonitorClient()
	if err != nil {
		return nil, monitor.Watch{}, err
	}
	watch, err := client.Watch(ctx, spec)
	if err != nil {
		return nil, monitor.Watch{}, err
	}
	if watch == nil {
		watch = &spec
	}
	return textResult("Now watching " + watch.Describe()), *watch, nil
}

type monitorUnwatchInput struct {
	ID   string `json:"id,omitempty" jsonschema:"The watch id from monitor_status. Takes precedence over the other fields."`
	Path string `json:"path,omitempty" jsonschema:"The working tree to stop following. Defaults to the server's working directory."`
	Repo string `json:"repo,omitempty" jsonschema:"The repository as owner/repo, with pr."`
	PR   int    `json:"pr,omitempty" jsonschema:"Pull request number, with repo."`
	URL  string `json:"url,omitempty" jsonschema:"A GitHub pull request URL."`
}

// unwatchDocument is the structured half of monitor_unwatch.
type unwatchDocument struct {
	ID string `json:"id"`
}

func monitorUnwatchTool(ctx context.Context, _ *mcp.CallToolRequest, in monitorUnwatchInput) (*mcp.CallToolResult, unwatchDocument, error) {
	id := in.ID
	if id == "" {
		spec, err := monitorWatchInput{Path: in.Path, Repo: in.Repo, PR: in.PR, URL: in.URL}.spec()
		if err != nil {
			return nil, unwatchDocument{}, err
		}
		id = spec.ID
	}
	client, err := newMonitorClient()
	if err != nil {
		return nil, unwatchDocument{}, err
	}
	client.AutoStart = false
	if err := client.Unwatch(ctx, id); err != nil {
		return nil, unwatchDocument{}, err
	}
	return textResult("Stopped watching " + id), unwatchDocument{ID: id}, nil
}

// spec turns the watch tool's inputs into a watch, sharing the CLI's
// resolution so the two front-ends agree on what "owner/repo 42" means.
func (in monitorWatchInput) spec() (monitor.Watch, error) {
	if in.URL != "" || in.Repo != "" || in.PR > 0 {
		args, err := prTargetArgs(in.URL, in.Repo, in.PR)
		if err != nil {
			return monitor.Watch{}, err
		}
		if args == nil {
			return monitor.Watch{}, fmt.Errorf("pass a url, or repo and pr, to watch a specific pull request")
		}
		return monitor.ParseWatchSpec(args, "")
	}
	dir := in.Path
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return monitor.Watch{}, fmt.Errorf("resolve the working directory: %w", err)
		}
	}
	return monitor.ParseWatchSpec(nil, dir)
}

type checkPinsInput struct {
	Path    string `json:"path,omitempty" jsonschema:"The checkout to audit. Defaults to the server's working directory."`
	Refresh bool   `json:"refresh,omitempty" jsonschema:"Ignore and rebuild the cached tag lists."`
	Offline bool   `json:"offline,omitempty" jsonschema:"List the references without resolving their latest releases — no network, and no suggested fixes."`
	Token   string `json:"token,omitempty" jsonschema:"GitHub token, overriding GITHUB_TOKEN/GH_TOKEN. Optional: public actions resolve unauthenticated."`
}

func checkPins(ctx context.Context, _ *mcp.CallToolRequest, in checkPinsInput) (*mcp.CallToolResult, pins.Document, error) {
	root := in.Path
	if root == "" {
		var err error
		if root, err = os.Getwd(); err != nil {
			return nil, pins.Document{}, fmt.Errorf("resolve the working directory: %w", err)
		}
	}
	report, err := cli.Pins(ctx, root, cli.PinsOptions{
		Token:   in.Token,
		Refresh: in.Refresh,
		Offline: in.Offline,
	})
	if err != nil {
		return nil, pins.Document{}, err
	}
	var b strings.Builder
	pins.Render(&b, report)
	return textResult(b.String()), pins.NewDocument(report), nil
}

// textResult wraps rendered text as an MCP tool result, matching how the
// inspection tools present their human-readable half.
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}
