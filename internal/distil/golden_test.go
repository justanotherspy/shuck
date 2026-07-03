package distil

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

var update = flag.Bool("update", false, "rewrite golden files instead of comparing")

// corpusJob mirrors the job.json fixture shape of the corpus cases.
type corpusJob struct {
	Name       string               `json:"name"`
	Conclusion string               `json:"conclusion"`
	Steps      []model.StepOverview `json:"steps"`
}

// TestCIFailureGolden distills every corpus case under testdata/ with the
// default options and compares the JSON-encoded Result against the case's
// committed golden. Regenerate (only when output is meant to change) with:
//
//	go test ./internal/distil -run Golden -update
func TestCIFailureGolden(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", e.Name(), "log.txt"))
			if err != nil {
				t.Fatalf("read log fixture: %v", err)
			}
			jobBytes, err := os.ReadFile(filepath.Join("testdata", e.Name(), "job.json"))
			if err != nil {
				t.Fatalf("read job fixture: %v", err)
			}
			var job corpusJob
			if err := json.Unmarshal(jobBytes, &job); err != nil {
				t.Fatalf("unmarshal job fixture: %v", err)
			}

			res, err := CIFailure(Input{
				JobName:       job.Name,
				JobConclusion: job.Conclusion,
				Steps:         job.Steps,
				RawLog:        string(raw),
				Options:       DefaultOptions(),
			})
			if err != nil {
				t.Fatalf("CIFailure: %v", err)
			}
			got, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			got = append(got, '\n')

			goldenPath := filepath.Join("testdata", e.Name(), "result.golden.json")
			if *update {
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden (run with -update to generate): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("result differs from golden %s\n--- got ---\n%s\n--- want ---\n%s",
					goldenPath, got, want)
			}
		})
	}
}
