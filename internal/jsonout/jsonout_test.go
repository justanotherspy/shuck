package jsonout

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestEncodeGolden(t *testing.T) {
	r := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 42, Title: "fix parser", HeadSHA: "abc123", HeadBranch: "feat"},
		FailedJobs: []model.JobResult{{
			ID: 7, RunID: 9, Name: "build", Conclusion: "failure",
			WorkflowName: "CI", WorkflowPath: ".github/workflows/ci.yml",
			// Inspected and CheckRunID are cache/internal fields and must not
			// appear in the stable JSON.
			Inspected: true, CheckRunID: 555,
			FailedSteps: []model.FailedStep{{
				Number: 3, Name: "Run tests", Kind: model.KindBash,
				Class: model.ClassTest, Command: "go test ./...", Excerpt: "--- FAIL",
			}},
			Annotations: []model.Annotation{{
				Path: "main_test.go", StartLine: 12, EndLine: 12, StartColumn: 5,
				Level: "failure", Message: "TestFoo failed",
			}},
		}},
		CancelledJobs: []model.JobResult{{
			ID: 8, RunID: 9, Name: "e2e", Conclusion: "cancelled",
			WorkflowName: "CI", WorkflowPath: ".github/workflows/ci.yml",
			Inspected: true,
			FailedSteps: []model.FailedStep{{
				Number: 2, Name: "Run e2e", Kind: model.KindBash,
				Command: "make e2e", Excerpt: "##[error]The operation was canceled.",
			}},
		}},
		OtherChecks: []model.OtherCheck{{Name: "lint", Conclusion: "failure", URL: "http://x"}},
		RunningJobs: []model.RunningJob{{Name: "deploy", Status: "in_progress", WorkflowName: "CD"}},
	}

	want := `{
  "schema_version": 1,
  "pr": {
    "owner": "o",
    "repo": "r",
    "number": 42,
    "title": "fix parser",
    "head_sha": "abc123",
    "head_branch": "feat"
  },
  "summary": {
    "failed": 1,
    "cancelled": 1,
    "running": 1,
    "other_failed": 1,
    "reviews": 0
  },
  "failed_jobs": [
    {
      "id": 7,
      "run_id": 9,
      "name": "build",
      "conclusion": "failure",
      "workflow_name": "CI",
      "workflow_path": ".github/workflows/ci.yml",
      "failed_steps": [
        {
          "number": 3,
          "name": "Run tests",
          "kind": "bash",
          "class": "test",
          "command": "go test ./...",
          "excerpt": "--- FAIL"
        }
      ],
      "annotations": [
        {
          "path": "main_test.go",
          "start_line": 12,
          "end_line": 12,
          "start_column": 5,
          "level": "failure",
          "message": "TestFoo failed"
        }
      ]
    }
  ],
  "cancelled_jobs": [
    {
      "id": 8,
      "run_id": 9,
      "name": "e2e",
      "conclusion": "cancelled",
      "workflow_name": "CI",
      "workflow_path": ".github/workflows/ci.yml",
      "failed_steps": [
        {
          "number": 2,
          "name": "Run e2e",
          "kind": "bash",
          "command": "make e2e",
          "excerpt": "##[error]The operation was canceled."
        }
      ],
      "annotations": []
    }
  ],
  "other_checks": [
    {
      "name": "lint",
      "conclusion": "failure",
      "url": "http://x"
    }
  ],
  "running_jobs": [
    {
      "name": "deploy",
      "status": "in_progress",
      "workflow_name": "CD"
    }
  ],
  "reviews": []
}`

	var buf strings.Builder
	if err := Encode(&buf, r); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != want {
		t.Errorf("JSON schema drifted.\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestEncodeRunTarget(t *testing.T) {
	r := &model.Report{
		Run: &model.RunInfo{
			Owner: "o", Repo: "r", RunID: 123, JobID: 456,
			Title: "fix parser", HeadSHA: "abc123", HeadBranch: "feat", WorkflowName: "CI",
		},
		FailedJobs: []model.JobResult{{ID: 456, Name: "build", Conclusion: "failure"}},
	}
	var buf strings.Builder
	if err := Encode(&buf, r); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out := buf.String()

	var doc Document
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	if doc.Run == nil {
		t.Fatalf("run target should populate the run object:\n%s", out)
	}
	if doc.Run.RunID != 123 || doc.Run.JobID != 456 || doc.Run.WorkflowName != "CI" {
		t.Errorf("unexpected run object: %+v", doc.Run)
	}
	if doc.Summary.Failed != 1 {
		t.Errorf("summary.failed = %d, want 1", doc.Summary.Failed)
	}
}

func TestEncodeOmitsRunForPRTarget(t *testing.T) {
	var buf strings.Builder
	if err := Encode(&buf, &model.Report{PR: model.PR{Owner: "o", Repo: "r", Number: 1}}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Contains(buf.String(), `"run"`) {
		t.Errorf("PR target must not emit a run object:\n%s", buf.String())
	}
}

func TestEncodeReviews(t *testing.T) {
	r := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 1},
		Reviews: []model.Review{{
			Author: "claude[bot]", AuthorType: model.AuthorAI, State: "changes_requested", Body: "fix",
			Threads: []model.ReviewThread{
				{Path: "a.go", Line: 4, TotalComments: 2, HiddenComments: 0, Comments: []model.ReviewComment{
					{Author: "claude[bot]", AuthorType: model.AuthorAI, Body: "nit"},
				}},
				{Path: "b.go", Line: 9, Resolved: true, Collapsed: true, CollapseReason: "resolved by bob"},
			},
		}},
	}
	var buf strings.Builder
	if err := Encode(&buf, r); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out := buf.String()

	var doc Document
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	if doc.Summary.Reviews != 1 {
		t.Errorf("summary.reviews = %d, want 1", doc.Summary.Reviews)
	}
	if len(doc.Reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(doc.Reviews))
	}
	rv := doc.Reviews[0]
	if rv.AuthorType != "ai" || rv.State != "changes_requested" {
		t.Errorf("review projection wrong: %+v", rv)
	}
	if len(rv.Threads) != 2 {
		t.Fatalf("want 2 threads, got %d", len(rv.Threads))
	}
	if !rv.Threads[1].Collapsed || rv.Threads[1].CollapseReason != "resolved by bob" {
		t.Errorf("collapsed thread projection wrong: %+v", rv.Threads[1])
	}
	if rv.Threads[1].Comments == nil {
		t.Errorf("comments should serialize as [] not null for collapsed thread")
	}
}

func TestEncodeEmptyListsAreNotNull(t *testing.T) {
	var buf strings.Builder
	if err := Encode(&buf, &model.Report{PR: model.PR{Owner: "o", Repo: "r", Number: 1}}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out := buf.String()

	// Must be valid JSON.
	var doc Document
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}

	for _, key := range []string{`"failed_jobs": []`, `"cancelled_jobs": []`, `"other_checks": []`, `"running_jobs": []`, `"reviews": []`} {
		if !strings.Contains(out, key) {
			t.Errorf("empty list should serialize as [] not null; missing %q in:\n%s", key, out)
		}
	}
}
