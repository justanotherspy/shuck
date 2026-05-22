package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestReportWithFailure(t *testing.T) {
	r := &model.Report{
		PR: model.PR{Owner: "justanotherspy", Repo: "shuck", Number: 12, Title: "add thing", HeadSHA: "abcdef1234567"},
		FailedJobs: []model.JobResult{{
			Name: "build", Conclusion: "failure", WorkflowName: "CI", WorkflowPath: ".github/workflows/ci.yml",
			Steps: []model.StepOverview{
				{Number: 1, Name: "Set up job", Conclusion: "success"},
				{Number: 2, Name: "Run tests", Conclusion: "failure"},
				{Number: 3, Name: "Upload", Conclusion: "skipped"},
			},
			FailedSteps: []model.FailedStep{{
				Number: 2, Name: "Run tests", Command: "go test ./...", Kind: model.KindBash, Excerpt: "--- FAIL: TestX",
			}},
		}},
		OtherChecks: []model.OtherCheck{{Name: "codecov/project", Conclusion: "failure", URL: "https://codecov.example"}},
		RunningJobs: []model.RunningJob{{Name: "integration", Status: "in_progress"}},
	}

	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()

	for _, want := range []string{
		"justanotherspy/shuck PR #12 — add thing",
		"commit abcdef1",
		"Workflow: CI (.github/workflows/ci.yml)",
		"Job: build  [failure]",
		"2. Run tests (failure)",
		"3. Upload (skipped)",
		"▸ Step 2 — Run tests (failed)",
		"* bash run:",
		"go test ./...",
		"--- FAIL: TestX",
		"Other checks (no logs available):",
		"codecov/project (failure) — https://codecov.example",
		"Still running:",
		`⏳ Job "integration" (in_progress)`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestReportAllGreen(t *testing.T) {
	r := &model.Report{PR: model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "abc1234"}}
	var buf bytes.Buffer
	Report(&buf, r)
	if !strings.Contains(buf.String(), "all checks passing") {
		t.Errorf("expected all-green message, got %q", buf.String())
	}
}

func TestReportActionLabel(t *testing.T) {
	r := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "abc1234"},
		FailedJobs: []model.JobResult{{
			Name: "build", Conclusion: "failure",
			FailedSteps: []model.FailedStep{{Name: "Checkout", Command: "actions/checkout@v4", Kind: model.KindAction, Excerpt: "boom"}},
		}},
	}
	var buf bytes.Buffer
	Report(&buf, r)
	if !strings.Contains(buf.String(), "* action called:") {
		t.Errorf("expected action label, got %q", buf.String())
	}
}
