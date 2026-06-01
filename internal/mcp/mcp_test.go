package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/justanotherspy/shuck/internal/action"
	"github.com/justanotherspy/shuck/internal/cache"
	"github.com/justanotherspy/shuck/internal/cli"
	"github.com/justanotherspy/shuck/internal/jsonout"
	"github.com/justanotherspy/shuck/internal/model"
)

func TestPRTargetArgs(t *testing.T) {
	cases := []struct {
		name      string
		url, repo string
		pr        int
		want      []string
		wantErr   bool
	}{
		{"url wins", "https://github.com/o/r/pull/1", "x/y", 9, []string{"https://github.com/o/r/pull/1"}, false},
		{"repo and pr", "", "o/r", 42, []string{"o/r", "42"}, false},
		{"pr only", "", "", 7, []string{"7"}, false},
		{"nothing", "", "", 0, nil, false},
		{"repo without pr", "", "o/r", 0, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := prTargetArgs(c.url, c.repo, c.pr)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err != nil {
				return
			}
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("args = %v, want %v", got, c.want)
			}
		})
	}
}

func TestInspectLogsRunTarget(t *testing.T) {
	cases := []struct {
		name             string
		run, repo        string
		wantRun, wantJob int64
		wantErr          bool
	}{
		{"run url", "https://github.com/o/r/actions/runs/123", "", 123, 0, false},
		{"job url", "https://github.com/o/r/actions/runs/123/job/456", "", 123, 456, false},
		{"pr url rejected", "https://github.com/o/r/pull/1", "", 0, 0, true},
		{"bare id with repo", "9", "o/r", 9, 0, false},
		{"bare id without repo", "9", "", 0, 0, true},
		{"bare id bad repo", "9", "norslash", 0, 0, true},
		{"not a run", "nope", "o/r", 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tgt, err := inspectLogsInput{Run: c.run, Repo: c.repo}.target()
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err != nil {
				return
			}
			if tgt.RunID != c.wantRun || tgt.JobID != c.wantJob {
				t.Errorf("run/job = %d/%d, want %d/%d", tgt.RunID, tgt.JobID, c.wantRun, c.wantJob)
			}
		})
	}
}

func TestExtractInputApplyDefaults(t *testing.T) {
	base := defaultOptions()
	// No overrides: defaults are preserved.
	if got := (extractInput{}).apply(base); got.Context != base.Context || got.ShortThreshold != base.ShortThreshold || got.Tail != base.Tail {
		t.Errorf("empty overrides changed defaults: %+v", got)
	}
	// Explicit zero context must override the default (pointer distinguishes it).
	zero := 0
	if got := (extractInput{Context: &zero}).apply(base); got.Context != 0 {
		t.Errorf("explicit context=0 not applied, got %d", got.Context)
	}
}

// TestRoundtripInspectLogsOffline drives the server end-to-end over an in-memory
// transport: it lists the tools (asserting they are advertised with typed
// input/output schemas) and calls inspect_logs against a seeded cache so no
// network or token is needed.
func TestRoundtripInspectLogsOffline(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	seed := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 42, Title: "fix parser", HeadSHA: "abc1234"},
		FailedJobs: []model.JobResult{{
			ID: 1, Name: "build", Conclusion: "failure", Inspected: true,
			FailedSteps: []model.FailedStep{{Number: 2, Name: "Run tests", Excerpt: "boom"}},
		}},
	}
	if err := cache.Save(seed); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	ctx := context.Background()
	cs := connectClient(ctx, t)

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tl := range tools.Tools {
		byName[tl.Name] = tl
	}
	for _, want := range []string{"inspect_logs", "inspect_reviews", "inspect_security", "check_compliance", "inspect_action", "inspect_images"} {
		tl, ok := byName[want]
		if !ok {
			t.Fatalf("tool %q not advertised; got %v", want, byName)
		}
		if tl.InputSchema == nil || tl.OutputSchema == nil {
			t.Errorf("tool %q missing input/output schema", want)
		}
	}
	for _, dead := range []string{"inspect_pr", "inspect_run"} {
		if _, ok := byName[dead]; ok {
			t.Errorf("retired tool %q should not be advertised", dead)
		}
	}
	if schema, _ := json.Marshal(byName["inspect_logs"].InputSchema); !strings.Contains(string(schema), "run") {
		t.Errorf("inspect_logs input schema missing 'run' property: %s", schema)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "inspect_logs",
		Arguments: inspectLogsInput{Repo: "o/r", PR: 42, Offline: true},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool reported error: %+v", res.Content)
	}

	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || len(res.Content) != 1 {
		t.Fatalf("want 1 text content block, got %d (%T)", len(res.Content), res.Content[0])
	}
	if !strings.Contains(text.Text, "build") || !strings.Contains(text.Text, "fix parser") {
		t.Errorf("rendered text missing expected report content:\n%s", text.Text)
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var doc jsonout.Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("structured content is not a jsonout.Document: %v\n%s", err, raw)
	}
	if doc.SchemaVersion != jsonout.SchemaVersion {
		t.Errorf("schema_version = %d, want %d", doc.SchemaVersion, jsonout.SchemaVersion)
	}
	if doc.Summary.Failed != 1 || len(doc.FailedJobs) != 1 || doc.FailedJobs[0].Name != "build" {
		t.Errorf("unexpected document: %+v", doc)
	}
}

// TestRoundtripInspectActionCached calls inspect_action against a seeded tag
// cache so the resolution runs without network.
// fakeTagLister serves a seeded default-branch SHA so the cached-resolution path
// runs without network. ListActionTags errors so the test fails loudly if the
// cache is unexpectedly bypassed.
type fakeTagLister struct{ sha string }

func (f fakeTagLister) ListActionTags(context.Context, string, string) ([]model.ActionTag, error) {
	return nil, fmt.Errorf("ListActionTags should not be called when the cache is warm")
}
func (f fakeTagLister) DefaultBranchSHA(context.Context, string, string) (string, error) {
	return f.sha, nil
}

func TestRoundtripInspectActionCached(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	const defaultSHA = "9999999999999999999999999999999999999999"
	if err := cache.SaveActionTags("actions", "checkout", defaultSHA, []model.ActionTag{
		{Name: "v4.2.0", SHA: "1111111111111111111111111111111111111111"},
		{Name: "v4.1.0", SHA: "2222222222222222222222222222222222222222"},
	}); err != nil {
		t.Fatalf("seed action cache: %v", err)
	}
	// Stub the GitHub client: the SHA check matches the seeded cache, so the
	// resolution reuses the cache and never touches the (erroring) tag list.
	prev := cli.NewTagLister
	cli.NewTagLister = func(string) cli.TagLister { return fakeTagLister{sha: defaultSHA} }
	t.Cleanup(func() { cli.NewTagLister = prev })

	ctx := context.Background()
	cs := connectClient(ctx, t)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "inspect_action",
		Arguments: inspectActionInput{Action: "actions/checkout@v4"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool reported error: %+v", res.Content)
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var doc action.Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("structured content is not an action.Document: %v\n%s", err, raw)
	}
	if doc.Tag != "v4.2.0" || doc.SHA != "1111111111111111111111111111111111111111" {
		t.Errorf("resolved tag/sha = %q/%q, want v4.2.0/1111…", doc.Tag, doc.SHA)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "actions/checkout") {
		t.Errorf("rendered text missing the action ref: %+v", res.Content)
	}
}

func connectClient(ctx context.Context, t *testing.T) *mcp.ClientSession {
	t.Helper()
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := newServer().Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
