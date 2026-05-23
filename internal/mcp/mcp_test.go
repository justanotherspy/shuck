package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/justanotherspy/shuck/internal/cache"
	"github.com/justanotherspy/shuck/internal/jsonout"
	"github.com/justanotherspy/shuck/internal/model"
)

func TestInspectPRTargetArgs(t *testing.T) {
	cases := []struct {
		name    string
		in      inspectPRInput
		want    []string
		wantErr bool
	}{
		{"url wins", inspectPRInput{URL: "https://github.com/o/r/pull/1", Repo: "x/y", PR: 9}, []string{"https://github.com/o/r/pull/1"}, false},
		{"repo and pr", inspectPRInput{Repo: "o/r", PR: 42}, []string{"o/r", "42"}, false},
		{"pr only", inspectPRInput{PR: 7}, []string{"7"}, false},
		{"nothing", inspectPRInput{}, nil, false},
		{"repo without pr", inspectPRInput{Repo: "o/r"}, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.in.targetArgs()
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

func TestInspectRunTarget(t *testing.T) {
	cases := []struct {
		name             string
		in               inspectRunInput
		wantRun, wantJob int64
		wantErr          bool
	}{
		{"run url", inspectRunInput{URL: "https://github.com/o/r/actions/runs/123"}, 123, 0, false},
		{"job url", inspectRunInput{URL: "https://github.com/o/r/actions/runs/123/job/456"}, 123, 456, false},
		{"pr url rejected", inspectRunInput{URL: "https://github.com/o/r/pull/1"}, 0, 0, true},
		{"repo and run_id", inspectRunInput{Repo: "o/r", RunID: 9, JobID: 8}, 9, 8, false},
		{"bad repo", inspectRunInput{Repo: "norslash", RunID: 9}, 0, 0, true},
		{"nothing", inspectRunInput{}, 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tgt, err := c.in.target()
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

// TestRoundtripInspectPROffline drives the server end-to-end over an in-memory
// transport: it lists the tools (asserting the typed input/output schemas are
// advertised) and calls inspect_pr against a seeded cache so no network or
// token is needed.
func TestRoundtripInspectPROffline(t *testing.T) {
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
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := newServer().Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = ss.Close() }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tl := range tools.Tools {
		byName[tl.Name] = tl
	}
	for _, want := range []string{"inspect_pr", "inspect_run"} {
		tl, ok := byName[want]
		if !ok {
			t.Fatalf("tool %q not advertised; got %v", want, byName)
		}
		if tl.InputSchema == nil || tl.OutputSchema == nil {
			t.Errorf("tool %q missing input/output schema", want)
		}
	}
	if schema, _ := json.Marshal(byName["inspect_pr"].InputSchema); !strings.Contains(string(schema), "repo") {
		t.Errorf("inspect_pr input schema missing 'repo' property: %s", schema)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "inspect_pr",
		Arguments: inspectPRInput{Repo: "o/r", PR: 42, Offline: true},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool reported error: %+v", res.Content)
	}

	// Text content is the rendered report.
	if len(res.Content) != 1 {
		t.Fatalf("want 1 content block, got %d", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *mcp.TextContent", res.Content[0])
	}
	if !strings.Contains(text.Text, "build") || !strings.Contains(text.Text, "fix parser") {
		t.Errorf("rendered text missing expected report content:\n%s", text.Text)
	}

	// Structured content is the stable JSON document.
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
