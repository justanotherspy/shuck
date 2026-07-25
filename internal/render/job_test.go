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
// annotations → step detail) are all part of what a reader relies on.
func TestJobBlock(t *testing.T) {
	var buf bytes.Buffer
	Job(&buf, drilledJob("failure"))

	want := strings.Join([]string{
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
	}, "\n")

	if got := buf.String(); got != want {
		t.Errorf("Job block mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	// Job renders one job and nothing report-level: no target header, no
	// summary counts. Embedders compose those themselves.
	for _, unwanted := range []string{"PR #", "Summary:", "Still running:"} {
		if strings.Contains(buf.String(), unwanted) {
			t.Errorf("Job emitted report-level chrome %q:\n%s", unwanted, buf.String())
		}
	}
}

// The reason Job is exported is that the monitor's event bodies must read
// exactly like the block `shuck logs` prints — not like a second, subtly
// different formatter. Assert that equivalence directly: the bytes Job writes
// have to appear verbatim inside a Report carrying that same job, through both
// of writeJob's callers (the failed-jobs loop and writeCancelled's drilled
// branch).
func TestJobMatchesReportVerbatim(t *testing.T) {
	tests := []struct {
		name       string
		conclusion string
		report     func(model.JobResult) *model.Report
		wantLabels []string
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
			wantLabels: []string{"Job: build  [failure]", "▸ Step 2 — Run tests (failed)", "error logs:"},
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
			// The conclusion-driven wording has to travel with the block, so a
			// monitor event about a cancelled job never claims it failed.
			wantLabels: []string{"Job: build  [cancelled]", "▸ Step 2 — Run tests (cancelled)", "logs before cancellation:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := drilledJob(tt.conclusion)

			var jb bytes.Buffer
			Job(&jb, job)
			block := jb.String()

			for _, want := range tt.wantLabels {
				if !strings.Contains(block, want) {
					t.Errorf("Job block missing %q\n---\n%s", want, block)
				}
			}

			var rb bytes.Buffer
			Report(&rb, tt.report(job))
			report := rb.String()

			if !strings.Contains(report, block) {
				t.Errorf("Job block is not verbatim in Report — the two formatters have drifted\n--- Job ---\n%s\n--- Report ---\n%s", block, report)
			}
			if n := strings.Count(report, block); n != 1 {
				t.Errorf("Job block appears %d times in Report, want exactly 1\n---\n%s", n, report)
			}
			// The report is the header, the summary and then the block: nothing
			// is appended after a lone job, so the block is the report's tail.
			if !strings.HasSuffix(report, block) {
				t.Errorf("Report should end with the job block\n--- Report ---\n%s", report)
			}
		})
	}
}
