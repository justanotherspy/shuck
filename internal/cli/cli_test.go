package cli

import (
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/logs"
	"github.com/justanotherspy/shuck/internal/model"
)

const failLog = `2024-05-01T10:00:00.0000000Z ##[group]Run actions/checkout@v4
2024-05-01T10:00:00.0000001Z ##[endgroup]
2024-05-01T10:00:01.0000000Z Synced
2024-05-01T10:00:02.0000000Z ##[group]Run go test ./...
2024-05-01T10:00:02.0000001Z go test ./...
2024-05-01T10:00:02.0000002Z ##[endgroup]
2024-05-01T10:00:03.0000000Z --- FAIL: TestThing
2024-05-01T10:00:04.0000000Z ##[error]Process completed with exit code 1.
`

func TestBuildFailedStepsAssociatesErrorSection(t *testing.T) {
	a := &app{opts: logs.DefaultOptions()}
	job := model.JobResult{Steps: []model.StepOverview{
		{Number: 1, Name: "Checkout", Conclusion: "success"},
		{Number: 2, Name: "Run tests", Conclusion: "failure"},
	}}
	fs := a.buildFailedSteps(job, failLog)
	if len(fs) != 1 {
		t.Fatalf("got %d failed steps, want 1: %+v", len(fs), fs)
	}
	if fs[0].Name != "Run tests" || fs[0].Number != 2 {
		t.Errorf("step name/number = %q/%d", fs[0].Name, fs[0].Number)
	}
	if fs[0].Command != "go test ./..." || fs[0].Kind != model.KindBash {
		t.Errorf("command/kind = %q/%q", fs[0].Command, fs[0].Kind)
	}
	if !strings.Contains(fs[0].Excerpt, "--- FAIL: TestThing") {
		t.Errorf("excerpt missing failure: %q", fs[0].Excerpt)
	}
}

func TestBuildFailedStepsFallbackNoErrorMarker(t *testing.T) {
	a := &app{opts: logs.DefaultOptions()}
	raw := "2024-05-01T10:00:00.0000000Z ##[group]Run go build\n2024-05-01T10:00:00.0000001Z ##[endgroup]\n2024-05-01T10:00:01.0000000Z some output\n"
	job := model.JobResult{Steps: []model.StepOverview{{Number: 1, Name: "Build", Conclusion: "failure"}}}
	fs := a.buildFailedSteps(job, raw)
	if len(fs) != 1 {
		t.Fatalf("got %d failed steps, want 1", len(fs))
	}
	if fs[0].Name != "Build" {
		t.Errorf("fallback should use the failed step name, got %q", fs[0].Name)
	}
	if !strings.Contains(fs[0].Excerpt, "some output") {
		t.Errorf("fallback excerpt should contain the log body: %q", fs[0].Excerpt)
	}
}

func TestResolveToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	if _, err := resolveToken(""); err == nil {
		t.Errorf("expected error when no token is set")
	}
	if got, _ := resolveToken("flagtok"); got != "flagtok" {
		t.Errorf("flag token should win, got %q", got)
	}
	t.Setenv("GH_TOKEN", "envtok")
	if got, _ := resolveToken(""); got != "envtok" {
		t.Errorf("GH_TOKEN fallback failed, got %q", got)
	}
}

func TestBuildExtractOptionsFull(t *testing.T) {
	opts, err := buildExtractOptions(options{shortThreshold: 100, context: 10, tail: 100, full: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.ShortThreshold <= 100 {
		t.Errorf("--full should raise the short threshold, got %d", opts.ShortThreshold)
	}

	if _, err := buildExtractOptions(options{pattern: "("}); err == nil {
		t.Errorf("expected error for invalid regexp")
	}
}

func TestExitFor(t *testing.T) {
	if exitFor(&model.Report{}) != 0 {
		t.Errorf("clean report should exit 0")
	}
	withFail := &model.Report{FailedJobs: []model.JobResult{{ID: 1}}}
	if exitFor(withFail) != 1 {
		t.Errorf("report with failures should exit 1")
	}
}
