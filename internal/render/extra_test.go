package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestReportReviewsOnlyEmpty(t *testing.T) {
	r := &model.Report{
		PR:          model.PR{Owner: "o", Repo: "r", Number: 3, HeadSHA: "abc1234"},
		ReviewsOnly: true,
	}
	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()
	if !strings.Contains(out, "(no reviews)") {
		t.Errorf("expected (no reviews), got %q", out)
	}
	// Reviews-only must not print the CI summary or all-clear lines.
	if strings.Contains(out, "Summary:") || strings.Contains(out, "all checks passing") {
		t.Errorf("reviews-only should not print CI sections:\n%s", out)
	}
}

func TestReportReviewsOnlyWithReviews(t *testing.T) {
	r := &model.Report{
		PR:          model.PR{Owner: "o", Repo: "r", Number: 4, HeadSHA: "abc1234"},
		ReviewsOnly: true,
		Reviews: []model.Review{
			{Author: "carol", AuthorType: model.AuthorHuman, State: "dismissed", Body: "stale review"},
		},
	}
	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()
	for _, want := range []string{"Reviews:", "⊘ dismissed — carol", "stale review"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "Summary:") {
		t.Errorf("reviews-only should not print a CI summary:\n%s", out)
	}
}

func TestReportStillRunningNoFailures(t *testing.T) {
	// Non-terminal report (a job in progress) with no failures: the "still
	// running" path renders instead of the all-clear line.
	r := &model.Report{
		PR:          model.PR{Owner: "o", Repo: "r", Number: 7, HeadSHA: "abc1234"},
		RunningJobs: []model.RunningJob{{Name: "build", Status: "in_progress"}},
	}
	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()
	for _, want := range []string{
		"⏳ no failures yet — some checks are still running",
		"Still running:",
		`⏳ Job "build" (in_progress)`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "all checks passing") {
		t.Errorf("a running report must not be reported all-clear:\n%s", out)
	}
}

func TestStepStateUsesStatusWhenNoConclusion(t *testing.T) {
	// A step still in progress has no Conclusion; stepState falls back to Status.
	r := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "abc1234"},
		FailedJobs: []model.JobResult{{
			Name: "build", Conclusion: "failure",
			Steps: []model.StepOverview{
				{Number: 1, Name: "Running step", Status: "in_progress"}, // no Conclusion
			},
			FailedSteps: []model.FailedStep{{Number: 1, Name: "Running step", Excerpt: "boom"}},
		}},
	}
	var buf bytes.Buffer
	Report(&buf, r)
	if !strings.Contains(buf.String(), "1. Running step (in_progress)") {
		t.Errorf("expected status-based step state, got:\n%s", buf.String())
	}
}

func TestCommandLabelDefault(t *testing.T) {
	// A failed step with an unknown step kind uses the generic "command" label.
	r := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "abc1234"},
		FailedJobs: []model.JobResult{{
			Name: "build", Conclusion: "failure",
			FailedSteps: []model.FailedStep{{
				Name: "mystery", Command: "do-thing", Kind: model.StepKind("weird"), Excerpt: "boom",
			}},
		}},
	}
	var buf bytes.Buffer
	Report(&buf, r)
	if !strings.Contains(buf.String(), "* command:") {
		t.Errorf("expected generic command label, got:\n%s", buf.String())
	}
}

func TestWriteOtherWithoutURL(t *testing.T) {
	r := &model.Report{
		PR:          model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "abc1234"},
		OtherChecks: []model.OtherCheck{{Name: "license/cla", Conclusion: "failure"}}, // no URL
	}
	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()
	if !strings.Contains(out, "✗ license/cla (failure)\n") {
		t.Errorf("expected URL-less other check line, got:\n%s", out)
	}
	// The URL-less branch omits the " — <url>" suffix.
	if strings.Contains(out, "license/cla (failure) —") {
		t.Errorf("URL-less line should not have a URL suffix:\n%s", out)
	}
}

func TestWriteThreadNoLineNoComments(t *testing.T) {
	// A thread with Line==0 prints just the path; with TotalComments==0 it still
	// prints the "(0 comments)" header and no comment bodies.
	r := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "abc1234"},
		Reviews: []model.Review{{
			Author: "bot", AuthorType: model.AuthorHuman, State: "commented",
			Threads: []model.ReviewThread{{Path: "go.mod", Line: 0, TotalComments: 0}},
		}},
	}
	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()
	if !strings.Contains(out, "▸ go.mod  (0 comments)") {
		t.Errorf("expected line-less thread header, got:\n%s", out)
	}
}

func TestWriteThreadSingleCommentNoPlural(t *testing.T) {
	r := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "abc1234"},
		Reviews: []model.Review{{
			Author: "bot", AuthorType: model.AuthorHuman, State: "commented",
			Threads: []model.ReviewThread{{
				Path: "main.go", Line: 1, TotalComments: 1, HiddenComments: 1,
				Comments: []model.ReviewComment{
					{Author: "x", AuthorType: model.AuthorHuman, Body: "line1\nline2"},
				},
			}},
		}},
	}
	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()
	// Singular "comment" (no trailing s) and multi-line comment continuation.
	if !strings.Contains(out, "(1 comment)") {
		t.Errorf("expected singular comment label, got:\n%s", out)
	}
	if !strings.Contains(out, "line2") {
		t.Errorf("expected multi-line continuation, got:\n%s", out)
	}
	if !strings.Contains(out, "… 1 more comment") {
		t.Errorf("expected singular hidden-comment note, got:\n%s", out)
	}
}

func TestReportRunTargetTitleFallsBackToWorkflow(t *testing.T) {
	// A run target with an empty Title uses the workflow name in the header.
	r := &model.Report{
		Run: &model.RunInfo{Owner: "o", Repo: "r", RunID: 12, HeadSHA: "deadbeefcafe", WorkflowName: "CI"},
	}
	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()
	if !strings.Contains(out, "o/r run 12 — CI") {
		t.Errorf("expected workflow-name title fallback, got:\n%s", out)
	}
}

func TestWriteCancelledAllDrilledNoBareListing(t *testing.T) {
	// Every cancelled job has drilled steps, so the "Cancelled (no logs
	// available)" bare listing is skipped entirely (the len(bare)==0 return).
	r := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "abc1234"},
		CancelledJobs: []model.JobResult{{
			Name: "e2e", Conclusion: "cancelled", WorkflowName: "CI",
			FailedSteps: []model.FailedStep{{Number: 2, Name: "run", Excerpt: "##[error]The operation was canceled."}},
		}},
	}
	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()
	if !strings.Contains(out, "Job: e2e  [cancelled]") {
		t.Errorf("drilled cancelled job should render, got:\n%s", out)
	}
	if strings.Contains(out, "Cancelled (no logs available):") {
		t.Errorf("no bare listing expected when all jobs are drilled:\n%s", out)
	}
}

func TestWriteCancelledBareNoWorkflowName(t *testing.T) {
	// A bare cancelled job with no workflow name uses the one-arg listing form.
	r := &model.Report{
		PR:            model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "abc1234"},
		CancelledJobs: []model.JobResult{{Name: "deploy", Conclusion: "cancelled"}}, // no WorkflowName
	}
	var buf bytes.Buffer
	Report(&buf, r)
	out := buf.String()
	if !strings.Contains(out, "⊘ deploy\n") {
		t.Errorf("expected workflow-less bare listing, got:\n%s", out)
	}
}

func TestShortSHAShortInput(t *testing.T) {
	// A short head SHA is printed verbatim (no slicing).
	r := &model.Report{PR: model.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "abc"}}
	var buf bytes.Buffer
	Report(&buf, r)
	if !strings.Contains(buf.String(), "commit abc)") {
		t.Errorf("expected verbatim short SHA, got:\n%s", buf.String())
	}
}
