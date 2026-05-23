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
			// Inspected is a cache-only field and must not appear in the JSON.
			Inspected: true,
			FailedSteps: []model.FailedStep{{
				Number: 3, Name: "Run tests", Kind: model.KindBash,
				Command: "go test ./...", Excerpt: "--- FAIL",
			}},
		}},
		CancelledJobs: []model.CancelledJob{{Name: "e2e", Conclusion: "cancelled", WorkflowName: "CI"}},
		OtherChecks:   []model.OtherCheck{{Name: "lint", Conclusion: "failure", URL: "http://x"}},
		RunningJobs:   []model.RunningJob{{Name: "deploy", Status: "in_progress", WorkflowName: "CD"}},
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
    "other_failed": 1
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
          "command": "go test ./...",
          "excerpt": "--- FAIL"
        }
      ]
    }
  ],
  "cancelled_jobs": [
    {
      "name": "e2e",
      "conclusion": "cancelled",
      "workflow_name": "CI"
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
  ]
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

	for _, key := range []string{`"failed_jobs": []`, `"cancelled_jobs": []`, `"other_checks": []`, `"running_jobs": []`} {
		if !strings.Contains(out, key) {
			t.Errorf("empty list should serialize as [] not null; missing %q in:\n%s", key, out)
		}
	}
}
