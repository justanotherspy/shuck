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
		CancelledJobs: []model.CancelledJob{{Name: "e2e", Conclusion: "cancelled", WorkflowName: "CI"}},
		OtherChecks:   []model.OtherCheck{{Name: "codecov/project", Conclusion: "failure", URL: "https://codecov.example"}},
		RunningJobs:   []model.RunningJob{{Name: "integration", Status: "in_progress"}},
	}

	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()

	for _, want := range []string{
		"justanotherspy/shuck PR #12 — add thing",
		"commit abcdef1",
		"Summary: 1 failed, 1 cancelled, 1 other failed, 1 running",
		"⚠ 1 still running — failures shown may be incomplete",
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
		"Cancelled (no logs drilled):",
		"⊘ e2e (CI)",
		"Still running:",
		`⏳ Job "integration" (in_progress)`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestReportCancelledOnly(t *testing.T) {
	r := &model.Report{
		PR:            model.PR{Owner: "o", Repo: "r", Number: 5, HeadSHA: "abc1234"},
		CancelledJobs: []model.CancelledJob{{Name: "deploy", Conclusion: "cancelled"}},
	}
	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()

	if strings.Contains(out, "all checks passing") {
		t.Errorf("a cancelled-only run must not be reported as all green:\n%s", out)
	}
	for _, want := range []string{"Summary: 1 cancelled", "Cancelled (no logs drilled):", "⊘ deploy"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	// No failures coexisting with running jobs, so there is no banner.
	if strings.Contains(out, "may be incomplete") {
		t.Errorf("unexpected banner with no running jobs:\n%s", out)
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

func TestReportReviews(t *testing.T) {
	r := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "abc1234"},
		Reviews: []model.Review{
			{
				Author: "coderabbitai[bot]", AuthorType: model.AuthorAI, State: "changes_requested",
				Body: "Please address the comments below.",
				Threads: []model.ReviewThread{
					{
						Path: "internal/cli/cli.go", Line: 42, TotalComments: 3, HiddenComments: 1,
						Comments: []model.ReviewComment{
							{Author: "coderabbitai[bot]", AuthorType: model.AuthorAI, Body: "needs a nil check"},
							{Author: "alice", AuthorType: model.AuthorHuman, Body: "agreed"},
						},
					},
					{Path: "internal/gh/gh.go", Line: 88, Collapsed: true, CollapseReason: "resolved by bob"},
					{Path: "README.md", Line: 10, Collapsed: true, CollapseReason: "outdated"},
				},
			},
			{Author: "ghbot", AuthorType: model.AuthorBot, State: "commented"},
			{Author: "dave", AuthorType: model.AuthorHuman, State: "approved", Body: "LGTM"},
		},
	}
	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()

	for _, want := range []string{
		"Reviews:",
		"✗ changes requested — coderabbitai[bot] [AI]",
		"Please address the comments below.",
		"▸ internal/cli/cli.go:42  (3 comments)",
		"coderabbitai[bot] [AI]: needs a nil check",
		"alice: agreed",
		"… 1 more comment",
		"▸ internal/gh/gh.go:88  (resolved by bob)",
		"▸ README.md:10  (outdated)",
		"💬 commented — ghbot [bot]",
		"✔ approved — dave",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reviews output missing %q\n---\n%s", want, out)
		}
	}
}

func TestReportRunTargetHeader(t *testing.T) {
	r := &model.Report{
		Run: &model.RunInfo{
			Owner: "o", Repo: "r", RunID: 123, JobID: 456,
			Title: "fix flaky parser", HeadSHA: "abcdef1234567", WorkflowName: "CI",
		},
		FailedJobs: []model.JobResult{{
			Name: "build", Conclusion: "failure", WorkflowName: "CI",
			FailedSteps: []model.FailedStep{{Number: 2, Name: "Run tests", Excerpt: "boom"}},
		}},
	}
	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()

	for _, want := range []string{
		"o/r job 456 (run 123) — fix flaky parser",
		"commit abcdef1",
		"Summary: 1 failed",
		"▸ Step 2 — Run tests (failed)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "PR #") {
		t.Errorf("run target should not print a PR header:\n%s", out)
	}
}

func TestReportRunTargetAllClear(t *testing.T) {
	r := &model.Report{Run: &model.RunInfo{Owner: "o", Repo: "r", RunID: 99, HeadSHA: "abc1234", Title: "CI"}}
	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()
	if !strings.Contains(out, "no failures in run 99") {
		t.Errorf("expected run-mode all-clear message, got %q", out)
	}
	if strings.Contains(out, "PR #") {
		t.Errorf("run target should not reference a PR:\n%s", out)
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
