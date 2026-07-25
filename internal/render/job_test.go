package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

// drilledJob is a job exercising everything writeJob emits: a workflow, a
// multi-step overview, an annotation, and one drilled step carrying both a
// command and a multi-line excerpt. conclusion drives the failed/cancelled
// wording.
func drilledJob(conclusion string) model.JobResult {
	return model.JobResult{
		Name: "build", Conclusion: conclusion,
		WorkflowName: "CI", WorkflowPath: ".github/workflows/ci.yml",
		Steps: []model.StepOverview{
			{Number: 1, Name: "Set up job", Conclusion: "success"},
			{Number: 2, Name: "Run tests", Conclusion: conclusion},
		},
		Annotations: []model.Annotation{
			{Path: "x_test.go", StartLine: 9, Level: "failure", Message: "TestX failed"},
		},
		FailedSteps: []model.FailedStep{{
			Number: 2, Name: "Run tests", Command: "go test ./...", Kind: model.KindBash,
			Class: model.ClassTest, Excerpt: "--- FAIL: TestX\n    x_test.go:9: boom",
		}},
	}
}

// The block Job writes is what a monitor event body shows an agent, so it is
// pinned byte-for-byte rather than by keyword: indentation, the fences around
// the command and the excerpt, and the order (workflow → job → steps →
// annotations → step detail) are all part of what a reader relies on. Byte
// equality is also what pins the negative: Job renders one job and no
// report-level chrome (no target header, no summary counts, no running list) —
// embedders compose those themselves.
func TestJobBlock(t *testing.T) {
	tests := []struct {
		name       string
		conclusion string
		want       []string
	}{
		{
			name:       "failed job",
			conclusion: "failure",
			want: []string{
				"", // writeJob opens with a blank line so blocks stack in a report
				"Workflow: CI (.github/workflows/ci.yml)",
				"Job: build  [failure]",
				"Steps:",
				"  1. Set up job (success)",
				"  2. Run tests (failure)",
				"Annotations:",
				"  ✗ x_test.go:9 — TestX failed",
				"",
				"  ▸ Step 2 — Run tests (failed) [test]",
				"    Step command:",
				"      * bash run:",
				"        ```",
				"        go test ./...",
				"        ```",
				"    error logs:",
				"      ```",
				"      --- FAIL: TestX",
				"          x_test.go:9: boom",
				"      ```",
				"",
			},
		},
		{
			// The cancellation wording is a property of the block itself, not of
			// the report that embeds it, so a monitor event about a cancelled job
			// never claims the step failed or that these were its error logs.
			name:       "cancelled job",
			conclusion: "cancelled",
			want: []string{
				"",
				"Workflow: CI (.github/workflows/ci.yml)",
				"Job: build  [cancelled]",
				"Steps:",
				"  1. Set up job (success)",
				"  2. Run tests (cancelled)",
				"Annotations:",
				"  ✗ x_test.go:9 — TestX failed",
				"",
				"  ▸ Step 2 — Run tests (cancelled) [test]",
				"    Step command:",
				"      * bash run:",
				"        ```",
				"        go test ./...",
				"        ```",
				"    logs before cancellation:",
				"      ```",
				"      --- FAIL: TestX",
				"          x_test.go:9: boom",
				"      ```",
				"",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			Job(&buf, drilledJob(tt.conclusion))

			want := strings.Join(tt.want, "\n")
			if got := buf.String(); got != want {
				t.Errorf("Job block mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

// stepBlock renders a job whose only interesting content is fs and returns the
// step detail alone — everything up to the "Steps:" line is the fixed chrome
// TestJobBlock already pins. It fails the test rather than returning a partial
// string, so a case can never silently assert against an empty block.
func stepBlock(t *testing.T, fs model.FailedStep) string {
	t.Helper()
	var buf bytes.Buffer
	Job(&buf, model.JobResult{
		Name: "build", Conclusion: "failure", WorkflowName: "CI", WorkflowPath: "ci.yml",
		FailedSteps: []model.FailedStep{fs},
	})
	_, detail, ok := strings.Cut(buf.String(), "Steps:\n")
	if !ok {
		t.Fatalf("job block lost its step overview header:\n%s", buf.String())
	}
	return detail
}

// The step detail carries the parts of a failure an agent acts on, and each of
// these shapes has its own suppression or trimming rule. Pinned byte-for-byte
// because the interesting content is what is *absent*: a substring search can
// tell you "Step command:" is present, never that a step with no command
// printed an empty fence anyway.
func TestFailedStepDetail(t *testing.T) {
	tests := []struct {
		name string
		step model.FailedStep
		want []string
	}{
		{
			// A step whose command could not be recovered from the log suppresses
			// the whole section — heading, kind label and fence — rather than
			// printing an empty code block for the reader to puzzle over.
			name: "no command drops the whole section",
			step: model.FailedStep{Number: 1, Name: "Run tests", Kind: model.KindBash, Class: model.ClassTest, Excerpt: "boom"},
			want: []string{
				"",
				"  ▸ Step 1 — Run tests (failed) [test]",
				"    error logs:",
				"      ```",
				"      boom",
				"      ```",
				"",
			},
		},
		{
			// An unclassified failure gets no tag at all. The zero FailureClass is
			// the empty string, so a classTag that stopped special-casing it would
			// append a bare " []" — noise that reads like a lost value.
			name: "unclassified failure carries no class tag",
			step: model.FailedStep{Number: 3, Name: "Build", Command: "make", Kind: model.KindBash, Excerpt: "boom"},
			want: []string{
				"",
				"  ▸ Step 3 — Build (failed)",
				"    Step command:",
				"      * bash run:",
				"        ```",
				"        make",
				"        ```",
				"    error logs:",
				"      ```",
				"      boom",
				"      ```",
				"",
			},
		},
		{
			// Job logs end with newlines; those are padding, not content, so they
			// must not push the closing fence away from the last real line.
			name: "trailing newlines in the excerpt are trimmed",
			step: model.FailedStep{Number: 1, Name: "Run tests", Excerpt: "boom\n\n\n"},
			want: []string{
				"",
				"  ▸ Step 1 — Run tests (failed)",
				"    error logs:",
				"      ```",
				"      boom",
				"      ```",
				"",
			},
		},
		{
			// An excerpt with no content still gets its fence, so the "error logs:"
			// heading is never left dangling over nothing.
			name: "empty excerpt still gets a fence",
			step: model.FailedStep{Number: 1, Name: "Run tests", Excerpt: ""},
			want: []string{
				"",
				"  ▸ Step 1 — Run tests (failed)",
				"    error logs:",
				"      ```",
				"      ",
				"      ```",
				"",
			},
		},
		{
			// Log text is attacker-influenced: a fence inside the excerpt is copied
			// through under the same indent, not escaped, re-fenced or truncated.
			name: "a fence inside the excerpt is copied through",
			step: model.FailedStep{Number: 1, Name: "Run tests", Excerpt: "```\n##[error]boom\n```"},
			want: []string{
				"",
				"  ▸ Step 1 — Run tests (failed)",
				"    error logs:",
				"      ```",
				"      ```",
				"      ##[error]boom",
				"      ```",
				"      ```",
				"",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := strings.Join(tt.want, "\n")
			if got := stepBlock(t, tt.step); got != want {
				t.Errorf("step detail mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
			}
		})
	}
}

// The reason Job is exported is that the monitor's event bodies must read
// exactly like the block `shuck logs` prints — not like a second, subtly
// different formatter. Assert that equivalence over the whole report rather
// than by containment: the report has to be its header, its summary and then
// Job's bytes, with nothing inserted before the block, nothing appended after
// it and no second copy. Both of writeJob's callers are covered (the
// failed-jobs loop and writeCancelled's drilled branch).
func TestJobMatchesReportVerbatim(t *testing.T) {
	tests := []struct {
		name       string
		conclusion string
		report     func(model.JobResult) *model.Report
		// wantChrome is everything the report prints before the job block,
		// spelled out literally so a change to the header or the summary counts
		// cannot hide behind the block comparison.
		wantChrome string
	}{
		{
			name:       "failed job",
			conclusion: "failure",
			report: func(j model.JobResult) *model.Report {
				return &model.Report{
					PR:         model.PR{Owner: "o", Repo: "r", Number: 1, Title: "add thing", HeadSHA: "abcdef1234567"},
					FailedJobs: []model.JobResult{j},
				}
			},
			wantChrome: "o/r PR #1 — add thing   (commit abcdef1)\n\nSummary: 1 failed\n",
		},
		{
			name:       "drilled cancelled job",
			conclusion: "cancelled",
			report: func(j model.JobResult) *model.Report {
				return &model.Report{
					PR:            model.PR{Owner: "o", Repo: "r", Number: 1, Title: "add thing", HeadSHA: "abcdef1234567"},
					CancelledJobs: []model.JobResult{j},
				}
			},
			wantChrome: "o/r PR #1 — add thing   (commit abcdef1)\n\nSummary: 1 cancelled\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := drilledJob(tt.conclusion)

			var jb bytes.Buffer
			Job(&jb, job)
			block := jb.String()

			var rb bytes.Buffer
			Report(&rb, tt.report(job))

			want := tt.wantChrome + block
			if got := rb.String(); got != want {
				t.Errorf("Report is not its chrome plus the Job block — the two formatters have drifted\n--- got ---\n%q\n--- want ---\n%q", got, want)
			}
		})
	}
}
