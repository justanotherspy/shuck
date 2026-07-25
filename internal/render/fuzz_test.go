package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

// FuzzRenderJobMatchesReport holds the exported Job block and the block Report
// prints for the same job to one formatter, whatever the job carries — log
// excerpts are attacker-influenced text (fences, CRs, control bytes, huge
// lines), and the monitor pastes Job's output into an event verbatim. Two
// invariants: Report contains Job's bytes exactly, and the excerpt survives the
// trip line for line, so no renderer ever quietly caps or rewrites the one part
// an agent reads for the error.
func FuzzRenderJobMatchesReport(f *testing.F) {
	f.Add("CI", ".github/workflows/ci.yml", "build", "failure", "Run tests", "go test ./...", "--- FAIL: TestX\n    x_test.go:9: boom")
	f.Add("", "", "", "cancelled", "", "", "")
	f.Add("CI", "a.yml", "j", "timed_out", "s", "```\nnested fence\n```", "```\n##[error]boom\n```")
	f.Add("w\nx", "p\ty", "j\r", "failure", "st\np", "cmd\r\n", "\n\n\n")

	f.Fuzz(func(t *testing.T, workflow, path, name, conclusion, step, command, excerpt string) {
		job := model.JobResult{
			Name: name, Conclusion: conclusion, WorkflowName: workflow, WorkflowPath: path,
			Steps:       []model.StepOverview{{Number: 1, Name: step, Conclusion: conclusion}},
			FailedSteps: []model.FailedStep{{Number: 1, Name: step, Command: command, Kind: model.KindBash, Excerpt: excerpt}},
		}

		var jb bytes.Buffer
		Job(&jb, job)
		block := jb.String()
		if !strings.HasPrefix(block, "\nWorkflow: ") {
			t.Fatalf("Job block lost its header: %q", block)
		}

		// Every excerpt line has to reach the fenced body under its indent.
		for line := range strings.SplitSeq(strings.TrimRight(excerpt, "\n"), "\n") {
			if !strings.Contains(block, "      "+line+"\n") {
				t.Fatalf("excerpt line %q missing from block:\n%s", line, block)
			}
		}

		var rb bytes.Buffer
		Report(&rb, &model.Report{
			PR:         model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "abcdef1234567"},
			FailedJobs: []model.JobResult{job},
		})
		if report := rb.String(); !strings.Contains(report, block) {
			t.Errorf("Job block is not verbatim in Report\n--- Job ---\n%q\n--- Report ---\n%q", block, report)
		}
	})
}
