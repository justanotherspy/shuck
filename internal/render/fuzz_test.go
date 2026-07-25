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
// lines), and the monitor pastes Job's output into an event verbatim. The
// oracles here are built without reusing writeFenced's own trim-then-split
// expression: an expectation spelled like the code under test moves with it and
// would bless a renderer that dropped, reordered, re-indented or capped the one
// part an agent reads for the error.
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

		// The excerpt fence is the last thing a block emits, so the whole excerpt
		// has to be its tail: one contiguous run, in order, every line under
		// exactly six spaces, nothing capped and nothing appended. Indenting the
		// input with a single ReplaceAll is deliberately a different computation
		// from the renderer's split loop, so the two cannot drift together.
		// Excerpts ending in a newline are trimmed first; the invariance check
		// below is what pins that, since re-running the trim here would not.
		if excerpt != "" && !strings.HasSuffix(excerpt, "\n") {
			want := "      ```\n      " + strings.ReplaceAll(excerpt, "\n", "\n      ") + "\n      ```\n"
			if !strings.HasSuffix(block, want) {
				t.Fatalf("excerpt did not reach the fence verbatim\n--- want suffix ---\n%q\n--- block ---\n%q", want, block)
			}
		}

		// Trailing newlines are padding, not content: appending them must not
		// move a byte of the block. Phrased as a differential between two renders
		// so it bites when the trim itself changes — an expected value computed
		// with that same trim never could.
		padded := job
		padded.FailedSteps = []model.FailedStep{{Number: 1, Name: step, Command: command, Kind: model.KindBash, Excerpt: excerpt + "\n\n"}}
		var pb bytes.Buffer
		Job(&pb, padded)
		if got := pb.String(); got != block {
			t.Errorf("trailing newlines in the excerpt changed the block\n--- plain ---\n%q\n--- padded ---\n%q", block, got)
		}

		// A step with no recovered command suppresses the whole "Step command:"
		// section. Probed on a job whose every other field is fixed, so a fuzzed
		// step name can never counterfeit the marker being searched for.
		var cb bytes.Buffer
		Job(&cb, model.JobResult{
			Name: "j", Conclusion: "failure", WorkflowName: "w", WorkflowPath: "p",
			FailedSteps: []model.FailedStep{{Number: 1, Name: "s", Command: command, Kind: model.KindBash, Excerpt: "boom"}},
		})
		if got, want := strings.Contains(cb.String(), "Step command:"), command != ""; got != want {
			t.Errorf("command %q: rendered a command section = %v, want %v\n%s", command, got, want, cb.String())
		}

		// Report is its header, its summary and then Job's bytes — pinned as an
		// equality so nothing can be slipped in ahead of the block or appended
		// after it, and so the block cannot appear twice.
		var rb bytes.Buffer
		Report(&rb, &model.Report{
			PR:         model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "abcdef1234567"},
			FailedJobs: []model.JobResult{job},
		})
		want := "o/r PR #1 —    (commit abcdef1)\n\nSummary: 1 failed\n" + block
		if got := rb.String(); got != want {
			t.Errorf("Report is not its chrome plus the Job block\n--- got ---\n%q\n--- want ---\n%q", got, want)
		}
	})
}
