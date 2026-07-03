package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/logs"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/render"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files instead of comparing")

// corpusJob mirrors the job.json fixture shape of the shared distillation
// corpus under internal/distil/testdata.
type corpusJob struct {
	Name       string               `json:"name"`
	Conclusion string               `json:"conclusion"`
	Steps      []model.StepOverview `json:"steps"`
}

// TestRenderedReportGoldenCorpus locks down the rendered CLI output for every
// case of the shared distillation corpus. The goldens were generated before
// the failed-step distillation moved into internal/distil, so this test
// passing unmodified is the proof that the refactor kept the CLI output
// byte-identical. Regenerate (only when output is meant to change) with:
//
//	go test ./internal/cli -run GoldenCorpus -update
func TestRenderedReportGoldenCorpus(t *testing.T) {
	const corpusDir = "../distil/testdata"
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			rawBytes, err := os.ReadFile(filepath.Join(corpusDir, e.Name(), "log.txt"))
			if err != nil {
				t.Fatalf("read log fixture: %v", err)
			}
			jobBytes, err := os.ReadFile(filepath.Join(corpusDir, e.Name(), "job.json"))
			if err != nil {
				t.Fatalf("read job fixture: %v", err)
			}
			var job corpusJob
			if err := json.Unmarshal(jobBytes, &job); err != nil {
				t.Fatalf("unmarshal job fixture: %v", err)
			}

			a := &app{opts: logs.DefaultOptions(), maxCommandLines: logs.DefaultMaxCommandLines}
			jr := model.JobResult{
				ID:           7001,
				RunID:        9001,
				Name:         job.Name,
				Status:       "completed",
				Conclusion:   job.Conclusion,
				RunAttempt:   1,
				WorkflowName: "CI",
				WorkflowPath: ".github/workflows/ci.yml",
				Steps:        job.Steps,
				Inspected:    true,
			}
			jr.FailedSteps = a.buildFailedSteps(jr, string(rawBytes))

			rep := model.Report{
				PR: model.PR{
					Owner:   "octo",
					Repo:    "hello",
					Number:  42,
					Title:   "Add feature",
					HeadSHA: "0123456789abcdef0123456789abcdef01234567",
				},
				CheckedAt: time.Date(2024, 5, 1, 10, 30, 0, 0, time.UTC),
			}
			if model.IsCancelledConclusion(job.Conclusion) {
				rep.CancelledJobs = []model.JobResult{jr}
			} else {
				rep.FailedJobs = []model.JobResult{jr}
			}

			var buf bytes.Buffer
			render.Report(&buf, &rep)

			goldenPath := filepath.Join("testdata", "golden", e.Name()+".report.golden.txt")
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatalf("mkdir golden dir: %v", err)
				}
				if err := os.WriteFile(goldenPath, buf.Bytes(), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden (run with -update to generate): %v", err)
			}
			if !bytes.Equal(buf.Bytes(), want) {
				t.Errorf("rendered report differs from golden %s\n--- got ---\n%s\n--- want ---\n%s",
					goldenPath, buf.String(), want)
			}
		})
	}
}
