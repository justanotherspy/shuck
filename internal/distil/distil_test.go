package distil

import (
	"strings"
	"testing"

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

func TestCIFailureAssociatesErrorSection(t *testing.T) {
	res, err := CIFailure(Input{
		JobConclusion: "failure",
		Steps: []model.StepOverview{
			{Number: 1, Name: "Checkout", Conclusion: "success"},
			{Number: 2, Name: "Run tests", Conclusion: "failure"},
		},
		RawLog:  failLog,
		Options: DefaultOptions(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fs := res.FailedSteps
	if len(fs) != 1 {
		t.Fatalf("got %d failed steps, want 1: %+v", len(fs), fs)
	}
	if fs[0].Name != "Run tests" || fs[0].Number != 2 {
		t.Errorf("step name/number = %q/%d", fs[0].Name, fs[0].Number)
	}
	if fs[0].Command != "go test ./..." || fs[0].Kind != model.KindBash {
		t.Errorf("command/kind = %q/%q", fs[0].Command, fs[0].Kind)
	}
	if fs[0].Class != model.ClassTest {
		t.Errorf("class = %q, want %q", fs[0].Class, model.ClassTest)
	}
	if !strings.Contains(fs[0].Excerpt, "--- FAIL: TestThing") {
		t.Errorf("excerpt missing failure: %q", fs[0].Excerpt)
	}
}

// cancelLog is a job that was cancelled mid-test-run: GitHub marks the
// interrupted step's section with "##[error]The operation was canceled." and
// the API marks that step (and every queued one after it) "cancelled".
const cancelLog = `2024-05-01T10:00:00.0000000Z ##[group]Run actions/checkout@v4
2024-05-01T10:00:00.0000001Z ##[endgroup]
2024-05-01T10:00:01.0000000Z Synced
2024-05-01T10:00:02.0000000Z ##[group]Run go test ./...
2024-05-01T10:00:02.0000001Z go test ./...
2024-05-01T10:00:02.0000002Z ##[endgroup]
2024-05-01T10:00:03.0000000Z ok   pkg/a  0.5s
2024-05-01T10:00:04.0000000Z ##[error]The operation was canceled.
`

func TestCIFailureCancelledJob(t *testing.T) {
	res, err := CIFailure(Input{
		JobConclusion: "cancelled",
		Steps: []model.StepOverview{
			{Number: 1, Name: "Checkout", Conclusion: "success"},
			{Number: 2, Name: "Run tests", Conclusion: "cancelled"},
			{Number: 3, Name: "Upload coverage", Conclusion: "cancelled"},
			{Number: 4, Name: "Notify", Conclusion: "cancelled"},
		},
		RawLog:  cancelLog,
		Options: DefaultOptions(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fs := res.FailedSteps

	// Only the step with an error section is reported; the queued-but-never-run
	// cancelled steps must not each emit a "(no matching section)" entry.
	if len(fs) != 1 {
		t.Fatalf("got %d steps, want 1: %+v", len(fs), fs)
	}
	if fs[0].Name != "Run tests" || fs[0].Number != 2 {
		t.Errorf("step name/number = %q/%d, want Run tests/2", fs[0].Name, fs[0].Number)
	}
	if fs[0].Command != "go test ./..." {
		t.Errorf("command = %q", fs[0].Command)
	}
	if !strings.Contains(fs[0].Excerpt, "The operation was canceled.") {
		t.Errorf("excerpt missing cancellation marker: %q", fs[0].Excerpt)
	}
}

func TestCIFailureCancelledJobNoErrorMarker(t *testing.T) {
	// A cancelled job whose log carries no ##[error] marker still gets the
	// whole-log fallback excerpt, named after the interrupted step.
	raw := "2024-05-01T10:00:00.0000000Z ##[group]Run make e2e\n2024-05-01T10:00:00.0000001Z ##[endgroup]\n2024-05-01T10:00:01.0000000Z spinning up fixtures\n"
	res, err := CIFailure(Input{
		JobConclusion: "cancelled",
		Steps:         []model.StepOverview{{Number: 1, Name: "Run e2e", Conclusion: "cancelled"}},
		RawLog:        raw,
		Options:       DefaultOptions(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fs := res.FailedSteps
	if len(fs) != 1 {
		t.Fatalf("got %d steps, want 1", len(fs))
	}
	if fs[0].Name != "Run e2e" {
		t.Errorf("fallback should use the interrupted step name, got %q", fs[0].Name)
	}
	if !strings.Contains(fs[0].Excerpt, "spinning up fixtures") {
		t.Errorf("fallback excerpt should contain the log body: %q", fs[0].Excerpt)
	}
}

func TestCIFailureFallbackNoErrorMarker(t *testing.T) {
	raw := "2024-05-01T10:00:00.0000000Z ##[group]Run go build\n2024-05-01T10:00:00.0000001Z ##[endgroup]\n2024-05-01T10:00:01.0000000Z some output\n"
	res, err := CIFailure(Input{
		JobConclusion: "failure",
		Steps:         []model.StepOverview{{Number: 1, Name: "Build", Conclusion: "failure"}},
		RawLog:        raw,
		Options:       DefaultOptions(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fs := res.FailedSteps
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

// multiStepLog is a job whose failing step ran a multi-line shell script, so the
// echoed command spans several Pre lines before the shell: metadata.
const multiStepLog = `2024-05-01T10:00:00.0000000Z ##[group]Run echo line1
2024-05-01T10:00:00.0000001Z echo line1
2024-05-01T10:00:00.0000002Z echo line2
2024-05-01T10:00:00.0000003Z echo line3
2024-05-01T10:00:00.0000004Z exit 1
2024-05-01T10:00:00.0000005Z shell: /usr/bin/bash -e {0}
2024-05-01T10:00:00.0000006Z ##[endgroup]
2024-05-01T10:00:01.0000000Z some output
2024-05-01T10:00:02.0000000Z ##[error]Process completed with exit code 1.
`

func TestCIFailureFullCommand(t *testing.T) {
	steps := []model.StepOverview{{Number: 1, Name: "Run script", Conclusion: "failure"}}

	// MaxCommandLines = 0 (no limit): the full multi-line script is recovered.
	opts := DefaultOptions()
	opts.MaxCommandLines = 0
	res, err := CIFailure(Input{JobConclusion: "failure", Steps: steps, RawLog: multiStepLog, Options: opts})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fs := res.FailedSteps
	wantFull := "echo line1\necho line2\necho line3\nexit 1"
	if len(fs) != 1 || fs[0].Command != wantFull {
		t.Fatalf("full command = %q, want %q", fs[0].Command, wantFull)
	}

	// A small cap truncates and reports how many lines were dropped.
	opts.MaxCommandLines = 2
	res, err = CIFailure(Input{JobConclusion: "failure", Steps: steps, RawLog: multiStepLog, Options: opts})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantClamped := "echo line1\necho line2\n… (2 more lines) …"
	if res.FailedSteps[0].Command != wantClamped {
		t.Errorf("clamped command = %q, want %q", res.FailedSteps[0].Command, wantClamped)
	}
}

// actionLog is a job whose failing step invoked an action, so GitHub echoed the
// step's with:/env: inputs into the group before the error.
const actionLog = `2024-05-01T10:00:00.0000000Z ##[group]Run actions/github-script@v7
2024-05-01T10:00:00.0000001Z with:
2024-05-01T10:00:00.0000002Z   script: throw new Error("boom")
2024-05-01T10:00:00.0000003Z   github-token: ***
2024-05-01T10:00:00.0000004Z env:
2024-05-01T10:00:00.0000005Z   NODE_ENV: test
2024-05-01T10:00:00.0000006Z ##[endgroup]
2024-05-01T10:00:01.0000000Z ##[error]Error: boom
`

func TestCIFailureActionInputs(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxCommandLines = 0
	res, err := CIFailure(Input{
		JobConclusion: "failure",
		Steps:         []model.StepOverview{{Number: 1, Name: "Run script action", Conclusion: "failure"}},
		RawLog:        actionLog,
		Options:       opts,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fs := res.FailedSteps
	if len(fs) != 1 || fs[0].Kind != model.KindAction {
		t.Fatalf("kind = %q, want action", fs[0].Kind)
	}
	want := "actions/github-script@v7\nwith:\n  script: throw new Error(\"boom\")\n  github-token: ***\nenv:\n  NODE_ENV: test"
	if fs[0].Command != want {
		t.Errorf("action command = %q, want %q", fs[0].Command, want)
	}
}

func TestCIFailureOptionValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Options)
	}{
		{"negative short threshold", func(o *Options) { o.Extract.ShortThreshold = -1 }},
		{"negative context", func(o *Options) { o.Extract.Context = -1 }},
		{"negative tail", func(o *Options) { o.Extract.Tail = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := DefaultOptions()
			tc.mut(&opts)
			if _, err := CIFailure(Input{RawLog: failLog, Options: opts}); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestDefaultOptions(t *testing.T) {
	opts := DefaultOptions()
	if opts.Extract.ShortThreshold != 100 || opts.Extract.Context != 10 || opts.Extract.Tail != 100 {
		t.Errorf("extract defaults = %+v", opts.Extract)
	}
	if opts.Extract.Pattern == nil {
		t.Errorf("default pattern must be set")
	}
	if opts.MaxCommandLines != 30 {
		t.Errorf("MaxCommandLines = %d, want 30", opts.MaxCommandLines)
	}
}

func TestSummaryFormat(t *testing.T) {
	res, err := CIFailure(Input{
		JobName:       "build-and-test",
		JobConclusion: "failure",
		Steps: []model.StepOverview{
			{Number: 1, Name: "Checkout", Conclusion: "success"},
			{Number: 2, Name: "Run tests", Conclusion: "failure"},
		},
		RawLog:  failLog,
		Options: DefaultOptions(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "build-and-test: failure — 1 failed step(s)\n- Run tests (step 2) [test]: --- FAIL: TestThing"
	if res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
}

func TestSummaryDefaultsAndTruncation(t *testing.T) {
	long := strings.Repeat("x", 130)
	raw := "##[group]Run go build\n##[endgroup]\nerror: " + long + "\n"
	res, err := CIFailure(Input{RawLog: raw, Options: DefaultOptions()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(res.Summary, "job: failure — 1 failed step(s)") {
		t.Errorf("summary header should use job/failure fallbacks: %q", res.Summary)
	}
	if !strings.HasSuffix(res.Summary, "…") {
		t.Errorf("long headline should be truncated with an ellipsis: %q", res.Summary)
	}
	for line := range strings.SplitSeq(res.Summary, "\n") {
		if n := len([]rune(line)); n > headlineMaxRunes+40 {
			t.Errorf("summary line too long (%d runes): %q", n, line)
		}
	}
}
