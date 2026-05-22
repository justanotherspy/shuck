package cache

import (
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())

	want := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 7, HeadSHA: "abc1234", Title: "fix"},
		FailedJobs: []model.JobResult{{
			ID: 1, RunAttempt: 1, Name: "build", Conclusion: "failure", Inspected: true,
			FailedSteps: []model.FailedStep{{Number: 3, Name: "test", Command: "go test ./...", Kind: model.KindBash, Excerpt: "FAIL"}},
		}},
	}
	if err := Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load("o", "r", 7)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil {
		t.Fatal("Load returned nil for a saved report")
	}
	if got.PR.HeadSHA != "abc1234" || len(got.FailedJobs) != 1 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.FailedJobs[0].FailedSteps[0].Command != "go test ./..." {
		t.Errorf("step command not preserved: %+v", got.FailedJobs[0].FailedSteps[0])
	}
}

func TestLoadMissingReturnsNil(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	got, err := Load("o", "r", 999)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing cache, got %+v", got)
	}
}

func TestInspectedJobs(t *testing.T) {
	r := &model.Report{FailedJobs: []model.JobResult{
		{ID: 1, RunAttempt: 1, Inspected: true},
		{ID: 2, RunAttempt: 2, Inspected: false},
	}}
	idx := InspectedJobs(r)
	if _, ok := idx[JobKey{ID: 1, RunAttempt: 1}]; !ok {
		t.Errorf("inspected job 1/1 should be indexed")
	}
	if _, ok := idx[JobKey{ID: 2, RunAttempt: 2}]; ok {
		t.Errorf("uninspected job 2/2 should not be indexed")
	}
	if got := len(InspectedJobs(nil)); got != 0 {
		t.Errorf("InspectedJobs(nil) len = %d, want 0", got)
	}
}
