package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/justanotherspy/shuck/internal/monitor"
)

// toolNames lists what the server advertises, through a real client session —
// the same view an agent gets.
func toolNames(t *testing.T) map[string]bool {
	t.Helper()
	ctx := context.Background()
	cs := connectClient(ctx, t)
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	out := map[string]bool{}
	for _, tool := range res.Tools {
		out[tool.Name] = true
	}
	return out
}

// resultText pulls the human-readable half out of a tool result.
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("tool result carried no content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("first content is %T, want text", res.Content[0])
	}
	return text.Text
}

// noMonitor points shuck's state at a temp directory and stops the tools from
// starting a daemon, which is the state these tests assert against.
func noMonitor(t *testing.T) {
	t.Helper()
	t.Setenv("SHUCK_HOME", t.TempDir())

	original := newMonitorClient
	newMonitorClient = func() (*monitor.Client, error) {
		c, err := monitor.NewClient()
		if err != nil {
			return nil, err
		}
		c.AutoStart = false
		return c, nil
	}
	t.Cleanup(func() { newMonitorClient = original })
}

func TestMonitorToolsAreRegistered(t *testing.T) {
	// The tools have to exist under exactly these names: the skill, the
	// plugin's permission list, and the docs all name them.
	want := []string{
		"inspect_logs", "inspect_reviews", "inspect_security",
		"check_compliance", "audit_dependabot", "inspect_action", "inspect_images",
		"monitor_status", "monitor_events", "monitor_watch", "monitor_unwatch", "check_pins",
	}
	got := toolNames(t)
	for _, name := range want {
		if !got[name] {
			t.Errorf("tool %q is not registered", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d tools registered, want %d — update the skill and the plugin's permission list too", len(got), len(want))
	}
}

func TestMonitorToolsReportNoDaemon(t *testing.T) {
	noMonitor(t)
	ctx := context.Background()

	if _, _, err := monitorStatusTool(ctx, nil, monitorStatusInput{}); err == nil {
		t.Error("monitor_status should report that no monitor is running")
	}
	if _, _, err := monitorEventsTool(ctx, nil, monitorEventsInput{Consumer: "s"}); err == nil {
		t.Error("monitor_events should report that no monitor is running")
	}
	if _, _, err := monitorUnwatchTool(ctx, nil, monitorUnwatchInput{ID: "tree:/x"}); err == nil {
		t.Error("monitor_unwatch should report that no monitor is running")
	}
}

func TestMonitorWatchSpec(t *testing.T) {
	tests := []struct {
		name   string
		in     monitorWatchInput
		wantID string
	}{
		{
			name:   "an explicit PR is pinned",
			in:     monitorWatchInput{Repo: "o/r", PR: 42},
			wantID: "pr:o/r#42",
		},
		{
			name:   "a URL is pinned",
			in:     monitorWatchInput{URL: "https://github.com/o/r/pull/42"},
			wantID: "pr:o/r#42",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.in.spec()
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tt.wantID)
			}
			if got.Kind != monitor.WatchPR {
				t.Errorf("Kind = %q, want a pinned PR watch", got.Kind)
			}
		})
	}

	t.Run("a path follows a working tree", func(t *testing.T) {
		dir := t.TempDir()
		got, err := monitorWatchInput{Path: dir}.spec()
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != monitor.WatchTree {
			t.Errorf("Kind = %q, want a tree watch", got.Kind)
		}
		abs, _ := filepath.Abs(dir)
		if got.Path != abs {
			t.Errorf("Path = %q, want %q", got.Path, abs)
		}
	})

	t.Run("no arguments follows the server's working directory", func(t *testing.T) {
		got, err := monitorWatchInput{}.spec()
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != monitor.WatchTree || got.Path == "" {
			t.Errorf("spec = %+v, want a tree watch on the working directory", got)
		}
	})

	t.Run("a repo without a PR number is rejected", func(t *testing.T) {
		if _, err := (monitorWatchInput{Repo: "o/r"}).spec(); err == nil {
			t.Error("repo without pr should be rejected rather than silently following the cwd")
		}
	})
}

func TestRenderMonitorStatus(t *testing.T) {
	got := renderMonitorStatus(&monitor.Status{
		Version: "v1.2.3",
		Watches: []monitor.Watch{{Kind: monitor.WatchTree, Path: "/w", Owner: "o", Repo: "r", Number: 7, Branch: "feature"}},
		Targets: []monitor.TargetStatus{
			{Target: "o/r#7", Verdict: "failed"},
			{Target: "o/r#8", Verdict: "passed", Lifecycle: "merged"},
			{Target: "o/r#9", LastError: "401 bad credentials"},
		},
		RateRemaining: 4200, RateLimit: 5000,
		Events: 12, Pending: 3,
	})

	for _, want := range []string{
		"v1.2.3", "/w", "o/r#7", "CHECKS FAILING", "all checks passed", "merged",
		"401 bad credentials", "4200/5000", "monitor_events",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered status is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderMonitorStatusEmpty(t *testing.T) {
	got := renderMonitorStatus(&monitor.Status{Version: "dev"})
	if !strings.Contains(got, "monitor_watch") {
		t.Errorf("an empty monitor should say how to point it at something:\n%s", got)
	}
}

func TestCheckPinsTool(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wf, "ci.yml"), []byte(`name: CI
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`), 0o600); err != nil {
		t.Fatal(err)
	}

	// offline keeps the tool off the network: it still reports what is
	// unpinned, just without a suggested fix.
	res, doc, err := checkPins(context.Background(), nil, checkPinsInput{Path: dir, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Summary.Unpinned != 1 {
		t.Errorf("Unpinned = %d, want 1", doc.Summary.Unpinned)
	}
	text := resultText(t, res)
	if !strings.Contains(text, "actions/checkout@v4") {
		t.Errorf("rendered report is missing the finding:\n%s", text)
	}
}

func TestCheckPinsToolRejectsAMissingPath(t *testing.T) {
	if _, _, err := checkPins(context.Background(), nil, checkPinsInput{Path: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Error("a missing directory should be reported")
	}
}
