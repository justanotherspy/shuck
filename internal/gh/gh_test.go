package gh

import (
	"testing"

	"github.com/google/go-github/v89/github"
)

func TestClassifyJobs(t *testing.T) {
	run := &github.WorkflowRun{
		ID:   new(int64(9)),
		Path: new(".github/workflows/ci.yml"),
	}
	jobs := []*github.WorkflowJob{
		{
			ID:         new(int64(1)),
			Name:       new("build"),
			Status:     new("completed"),
			Conclusion: new("failure"),
			Steps: []*github.TaskStep{
				{Number: new(int64(1)), Name: new("Run tests"), Conclusion: new("failure")},
			},
		},
		{
			ID:         new(int64(2)),
			Name:       new("e2e"),
			Status:     new("completed"),
			Conclusion: new("cancelled"),
			Steps: []*github.TaskStep{
				{Number: new(int64(1)), Name: new("Run e2e"), Conclusion: new("cancelled")},
			},
		},
		{
			ID:         new(int64(3)),
			Name:       new("lint"),
			Status:     new("completed"),
			Conclusion: new("success"),
		},
		{
			ID:     new(int64(4)),
			Name:   new("deploy"),
			Status: new("in_progress"),
		},
	}

	failed, cancelled, running := classifyJobs(run, jobs)

	if len(failed) != 1 || failed[0].Name != "build" || failed[0].RunID != 9 {
		t.Errorf("failed = %+v, want one job named build with run id 9", failed)
	}
	// Cancelled jobs must be drillable: full JobResult with ID and step overview.
	if len(cancelled) != 1 {
		t.Fatalf("cancelled = %+v, want one job", cancelled)
	}
	if cancelled[0].ID != 2 || cancelled[0].Conclusion != "cancelled" || len(cancelled[0].Steps) != 1 {
		t.Errorf("cancelled job missing drillable detail: %+v", cancelled[0])
	}
	if len(running) != 1 || running[0].Name != "deploy" {
		t.Errorf("running = %+v, want one job named deploy", running)
	}
}
